// Command dispatcher is configuration B's worker: it takes queued resource
// calls from Redis (internal/resource.RedisQueue) and runs them.
//
// For each resource in RESOURCES (name=concurrency,...) it keeps at most that
// many calls in flight, takes calls round-robin across tenants, and on a 429
// or 503 puts the call back at the head of its list and pauses that resource
// for Retry-After (or a jittered backoff). Final answers are stored under the
// call's key and announced to the waiting agent.
//
// Environment: REDIS_URL (redis://127.0.0.1:6379), RESOURCES (llm=8,db=8,sandbox=4),
// LISTEN (:8300, for /metrics), CALL_TIMEOUT (10m).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/resource"
)

type pool struct {
	name    string
	limit   int
	rdb     *redis.Client
	http    *http.Client
	timeout time.Duration

	mu       sync.Mutex
	cond     *sync.Cond
	inflight int
	paused   time.Time
	backoff  time.Duration
	tenants  []string
	next     int

	taken, requests, requeued, results atomic.Int64
}

func main() {
	opt, err := redis.ParseURL(env("REDIS_URL", "redis://127.0.0.1:6379"))
	if err != nil {
		log.Fatalf("REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opt)
	timeout, err := time.ParseDuration(env("CALL_TIMEOUT", "10m"))
	if err != nil {
		log.Fatalf("CALL_TIMEOUT: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
	var pools []*pool
	for _, spec := range strings.Split(env("RESOURCES", "llm=8,db=8,sandbox=4"), ",") {
		name, n, _ := strings.Cut(strings.TrimSpace(spec), "=")
		limit, err := strconv.Atoi(n)
		if err != nil || limit < 1 {
			log.Fatalf("RESOURCES: bad entry %q", spec)
		}
		p := &pool{name: name, limit: limit, rdb: rdb, http: client, timeout: timeout, backoff: 200 * time.Millisecond}
		p.cond = sync.NewCond(&p.mu)
		pools = append(pools, p)
		go p.refreshTenants()
		go p.run()
	}
	http.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		for _, p := range pools {
			p.mu.Lock()
			fmt.Fprintf(w, "dispatcher_inflight{resource=%q} %d\n", p.name, p.inflight)
			p.mu.Unlock()
			fmt.Fprintf(w, "dispatcher_taken_total{resource=%q} %d\ndispatcher_requests_total{resource=%q} %d\n", p.name, p.taken.Load(), p.name, p.requests.Load())
			fmt.Fprintf(w, "dispatcher_requeued_total{resource=%q} %d\ndispatcher_results_total{resource=%q} %d\n", p.name, p.requeued.Load(), p.name, p.results.Load())
		}
	})
	addr := env("LISTEN", ":8300")
	log.Printf("dispatcher on %s: %s", addr, env("RESOURCES", "llm=8,db=8,sandbox=4"))
	log.Fatal(http.ListenAndServe(addr, nil))
}

// refreshTenants keeps the list of tenants with queued calls, every second.
func (p *pool) refreshTenants() {
	for {
		ts, err := p.rdb.SMembers(context.Background(), "q:tenants:"+p.name).Result()
		if err == nil {
			sort.Strings(ts)
			p.mu.Lock()
			p.tenants = ts
			p.mu.Unlock()
		}
		time.Sleep(time.Second)
	}
}

// run takes calls while there is room in flight and the resource isn't paused.
func (p *pool) run() {
	ctx := context.Background()
	for {
		p.mu.Lock()
		for p.inflight >= p.limit {
			p.cond.Wait()
		}
		wait := time.Until(p.paused)
		keys := make([]string, 0, len(p.tenants))
		for i := range p.tenants {
			keys = append(keys, fmt.Sprintf("q:%s:%s", p.name, p.tenants[(p.next+i)%len(p.tenants)]))
		}
		p.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
			continue
		}
		if len(keys) == 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		got, err := p.rdb.BLPop(ctx, time.Second, keys...).Result()
		if err != nil {
			if err != redis.Nil {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		var job resource.Job
		if json.Unmarshal([]byte(got[1]), &job) != nil {
			continue
		}
		p.mu.Lock()
		p.inflight++
		p.taken.Add(1)
		// The next take starts after this tenant, so each gets its turn.
		for i, t := range p.tenants {
			if t == job.Tenant {
				p.next = i + 1
			}
		}
		p.mu.Unlock()
		go p.send(job, got[0])
	}
}

func (p *pool) send(job resource.Job, list string) {
	defer func() {
		p.mu.Lock()
		p.inflight--
		p.cond.Signal()
		p.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	p.requests.Add(1)
	req, _ := http.NewRequestWithContext(ctx, job.Method, job.URL, bytes.NewReader(job.Body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", job.Key)
	resp, err := p.http.Do(req)
	status, retryAfter := 0, time.Duration(0)
	var body []byte
	if err == nil {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		status = resp.StatusCode
		if n, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && n > 0 {
			retryAfter = time.Duration(n) * time.Second
		}
	}
	if err != nil || status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		// Back to the head of its list, and pause this resource.
		p.mu.Lock()
		wait := time.Duration(rand.Int64N(int64(p.backoff) + 1))
		if retryAfter > wait {
			wait = retryAfter
		}
		if until := time.Now().Add(wait); until.After(p.paused) {
			p.paused = until
		}
		p.backoff = min(p.backoff*2, 10*time.Second)
		p.mu.Unlock()
		raw, _ := json.Marshal(job)
		for p.rdb.LPush(context.Background(), list, raw).Err() != nil {
			time.Sleep(100 * time.Millisecond)
		}
		p.requeued.Add(1)
		return
	}
	p.mu.Lock()
	p.backoff = 200 * time.Millisecond
	p.mu.Unlock()
	res, _ := json.Marshal(resource.JobResult{Status: status, Body: body, Attempts: 1})
	for {
		_, err := p.rdb.TxPipelined(context.Background(), func(pp redis.Pipeliner) error {
			pp.Set(context.Background(), "q:result:"+job.Key, res, time.Hour)
			pp.Publish(context.Background(), "q:done:"+job.Key, "1")
			return nil
		})
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.results.Add(1)
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
