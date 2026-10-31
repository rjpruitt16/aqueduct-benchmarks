// Package resource is how an agent reaches a constrained resource (the LLM,
// the database tool service, the sandbox). The agent code is identical in
// every configuration; only the Client's mode differs:
//
//	direct   call the resource, retrying refusals and errors with exponential
//	         backoff and full jitter, honoring Retry-After (configuration A)
//	queue    put the call on the resource's Redis queue and wait for a
//	         dispatcher to run it (configuration B)
//	aquifer  send the call through Aquifer's /proxy, which queues and paces it
//	         when the resource is overloaded (configuration C)
//
// Every call carries an idempotency key, so a repeated call for the same
// workflow step can be recognized wherever the mode allows.
package resource

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Call is one request to a resource.
type Call struct {
	Resource string // llm, db or sandbox: for metrics and the queue's name
	Method   string
	URL      string
	Body     []byte
	Key      string // idempotency key: one per workflow step and attempt
	Tenant   string
}

// Result is the resource's final answer, and what it took to get it.
type Result struct {
	Status   int
	Body     []byte
	Attempts int           // requests this client sent for the call
	Backoff  time.Duration // time spent waiting between them
}

// Client reaches resources in one mode.
type Client struct {
	Mode       string // direct, queue, aquifer
	HTTP       *http.Client
	AquiferURL string // aquifer mode
	Queue      Queue  // queue mode
	// Direct-mode backoff: full jitter on BaseBackoff doubling to MaxBackoff.
	BaseBackoff, MaxBackoff time.Duration

	Requests atomic.Int64 // every request sent, for requests per workflow
}

// Queue is configuration B's queue: Submit hands a call to the dispatchers
// and returns its result once one has run it.
type Queue interface {
	Submit(ctx context.Context, c Call) (Result, error)
}

// Do runs a call until the resource answers with a final status (anything
// but 429, 503 or a transport error), or ctx ends.
func (c *Client) Do(ctx context.Context, call Call) (Result, error) {
	switch c.Mode {
	case "direct":
		return c.direct(ctx, call)
	case "aquifer":
		return c.aquifer(ctx, call)
	case "queue":
		if c.Queue == nil {
			return Result{}, errors.New("queue mode without a queue")
		}
		return c.Queue.Submit(ctx, call)
	}
	return Result{}, fmt.Errorf("unknown resource mode %q", c.Mode)
}

// retryable reports whether a status means "come back later".
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

func (c *Client) direct(ctx context.Context, call Call) (Result, error) {
	var res Result
	backoff := c.BaseBackoff
	for {
		res.Attempts++
		status, body, retryAfter, err := c.send(ctx, call.Method, call.URL, call.Body, map[string]string{"Idempotency-Key": call.Key})
		if err == nil && !retryable(status) {
			res.Status, res.Body = status, body
			return res, nil
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		wait := time.Duration(rand.Int64N(int64(backoff) + 1))
		if retryAfter > wait {
			wait = retryAfter
		}
		if err := sleep(ctx, wait); err != nil {
			return res, err
		}
		res.Backoff += wait
		if backoff < c.MaxBackoff {
			backoff = min(backoff*2, c.MaxBackoff)
		}
	}
}

// aquifer sends the call to Aquifer's /proxy. Aquifer tries the resource at
// once and relays a direct answer; if the resource is overloaded it queues the
// call and the connection becomes an event stream that ends in its result. A
// refusal from Aquifer itself (429) or a dropped stream is retried with the
// same key, which reattaches to the same job.
func (c *Client) aquifer(ctx context.Context, call Call) (Result, error) {
	var res Result
	job := map[string]any{
		"user_id": call.Tenant, "idempotent_key": call.Key, "url": call.URL, "method": call.Method,
		"headers": map[string]string{"Content-Type": "application/json", "Idempotency-Key": call.Key},
		"body":    string(call.Body), "max_retries": -1,
	}
	payload, _ := json.Marshal(job)
	backoff := c.BaseBackoff
	for {
		res.Attempts++
		status, body, retryAfter, done, err := c.proxyOnce(ctx, payload)
		if err == nil && done {
			res.Status, res.Body = status, body
			return res, nil
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		wait := time.Duration(rand.Int64N(int64(backoff) + 1))
		if retryAfter > wait {
			wait = retryAfter
		}
		if err := sleep(ctx, wait); err != nil {
			return res, err
		}
		res.Backoff += wait
		if backoff < c.MaxBackoff {
			backoff = min(backoff*2, c.MaxBackoff)
		}
	}
}

// proxyOnce makes one /proxy request. done is true when it ends with the
// resource's final answer.
func (c *Client) proxyOnce(ctx context.Context, payload []byte) (status int, body []byte, retryAfter time.Duration, done bool, err error) {
	c.Requests.Add(1)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.AquiferURL+"/proxy", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, 0, false, err
	}
	defer resp.Body.Close()
	retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			return resp.StatusCode, raw, retryAfter, false, nil // Aquifer itself refused
		}
		// A duplicate of a job Aquifer already holds: its result is on the job.
		var dup struct {
			JobID     string `json:"job_id"`
			Duplicate bool   `json:"duplicate"`
		}
		if json.Unmarshal(raw, &dup) == nil && dup.Duplicate && dup.JobID != "" {
			return c.storedResult(ctx, dup.JobID)
		}
		return resp.StatusCode, raw, 0, true, nil // the resource's own answer, relayed
	}
	// Queued: read events until the job's outcome.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:") && (event == "completed" || event == "failed"):
			var out struct {
				ResponseStatus int    `json:"response_status"`
				Body           string `json:"body"`
				Reason         string `json:"reason"`
			}
			json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &out)
			if event == "failed" {
				return out.ResponseStatus, []byte(out.Reason), 0, true, nil
			}
			return out.ResponseStatus, []byte(out.Body), 0, true, nil
		}
	}
	return 0, nil, 0, false, errors.New("stream ended without an outcome")
}

// storedResult reads a finished job's result; done is false while it hasn't
// finished, so the caller comes back.
func (c *Client) storedResult(ctx context.Context, jobID string) (int, []byte, time.Duration, bool, error) {
	status, raw, retryAfter, err := c.send(ctx, http.MethodGet, c.AquiferURL+"/jobs/"+jobID, nil, nil)
	if err != nil || status != http.StatusOK {
		return status, raw, retryAfter, false, err
	}
	var job struct {
		Status string `json:"status"`
		Result *struct {
			ResponseStatus int    `json:"response_status"`
			Body           string `json:"body"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &job) != nil || job.Result == nil {
		return 0, nil, time.Second, false, nil
	}
	return job.Result.ResponseStatus, []byte(job.Result.Body), 0, true, nil
}

func (c *Client) send(ctx context.Context, method, url string, body []byte, headers map[string]string) (int, []byte, time.Duration, error) {
	c.Requests.Add(1)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return min(time.Duration(n)*time.Second, time.Minute)
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
