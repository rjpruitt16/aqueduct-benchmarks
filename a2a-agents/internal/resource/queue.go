package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisQueue is configuration B, the agent's side: a call goes onto its
// resource's list for its tenant, once per idempotency key, and the agent
// waits for a dispatcher (cmd/dispatcher) to publish the result.
//
// Keys:
//
//	q:{resource}:{tenant}   list of pending calls
//	q:tenants:{resource}    set of tenants with calls for the resource
//	q:claim:{key}           set once when a key is first queued
//	q:result:{key}          the call's final status and body (kept an hour)
//	q:done:{key}            pub/sub channel announcing the result
type RedisQueue struct {
	rdb *redis.Client
}

// Job is a queued call, as the dispatcher reads it.
type Job struct {
	Resource string `json:"resource"`
	Method   string `json:"method"`
	URL      string `json:"url"`
	Body     []byte `json:"body"`
	Key      string `json:"key"`
	Tenant   string `json:"tenant"`
	Queued   int64  `json:"queued_ms"`
}

// JobResult is what the dispatcher stores for a key.
type JobResult struct {
	Status   int    `json:"status"`
	Body     []byte `json:"body"`
	Attempts int    `json:"attempts"` // the dispatcher's requests to the resource
}

func NewRedisQueue(url string) (*RedisQueue, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	return &RedisQueue{rdb: redis.NewClient(opt)}, nil
}

// Submit queues the call unless its key is already queued or done, then
// waits for its result.
func (q *RedisQueue) Submit(ctx context.Context, c Call) (Result, error) {
	// Subscribe before checking for a result, so one published in between
	// isn't missed.
	sub := q.rdb.Subscribe(ctx, "q:done:"+c.Key)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		return Result{}, err
	}
	if r, ok, err := q.result(ctx, c.Key); err != nil || ok {
		return r, err
	}
	fresh, err := q.rdb.SetNX(ctx, "q:claim:"+c.Key, time.Now().UnixMilli(), time.Hour).Result()
	if err != nil {
		return Result{}, err
	}
	if fresh {
		job, _ := json.Marshal(Job{Resource: c.Resource, Method: c.Method, URL: c.URL, Body: c.Body, Key: c.Key, Tenant: c.Tenant,
			Queued: time.Now().UnixMilli()})
		if _, err := q.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.RPush(ctx, fmt.Sprintf("q:%s:%s", c.Resource, c.Tenant), job)
			p.SAdd(ctx, "q:tenants:"+c.Resource, c.Tenant)
			return nil
		}); err != nil {
			q.rdb.Del(context.WithoutCancel(ctx), "q:claim:"+c.Key)
			return Result{}, err
		}
	}
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			// The result may have landed just as the wait ended.
			if r, ok, _ := q.result(context.WithoutCancel(ctx), c.Key); ok {
				return r, nil
			}
			return Result{}, ctx.Err()
		case <-ch:
			if r, ok, err := q.result(ctx, c.Key); err != nil || ok {
				return r, err
			}
		}
	}
}

func (q *RedisQueue) result(ctx context.Context, key string) (Result, bool, error) {
	raw, err := q.rdb.Get(ctx, "q:result:"+key).Bytes()
	if err == redis.Nil {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	var jr JobResult
	if err := json.Unmarshal(raw, &jr); err != nil {
		return Result{}, false, err
	}
	// Attempts counts one request from the agent to the queue; the
	// dispatcher's own requests are in its metrics.
	return Result{Status: jr.Status, Body: jr.Body, Attempts: 1}, true, nil
}
