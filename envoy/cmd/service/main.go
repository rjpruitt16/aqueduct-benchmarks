// Command service is one microservice in a chain that ends at the backend,
// for the wild regime:
//
//	api ──> orders ──┬──> inventory ──┐
//	                 └──> payments ───┴──> front door ──> backend
//
// Each POST /req is passed down: api calls orders, orders calls inventory
// and payments together and answers when both are done, and inventory and
// payments each run one backend job through the front door. Every service
// retries what it calls the way an ordinary service client does: ATTEMPTS
// tries, RETRY_DELAYS apart with no jitter, each bounded by TIMEOUT, then a
// 503 to its own caller, which retries in turn. A caller that gives up
// cancels the call below it (the request context is passed down), but work
// already queued at the backend still runs.
//
// The bottom services reach the backend in one of two ways (MODE):
//
//	envoy    POST FRONT_URL/work, Envoy in front of the backend
//	aquifer  POST FRONT_URL/proxy, Aquifer in front of the backend: the call
//	         waits for the job's result, and a retry with the same
//	         idempotency key joins the job already held instead of running
//	         it again
//	envoy-idem   Envoy, behind an idempotency check in Redis, the
//	         Stripe-style pattern: a finished job is answered from its stored
//	         result, and a repeat of a job that is running waits for it
//	         instead of calling the backend
//	envoy-queue  idempotency plus a fair queue in Redis: the job is queued
//	         once on its tenant's list, every call waits for its result, and
//	         cmd/dispatcher sends it to the backend through Envoy
//
// The job's ID is the request's ID and the service's name, so every retry,
// at any level, asks for the same job, and the backend can count repeats.
//
// Every request received and sent is counted, and the counts are POSTed to
// REPORT_URL/svc-NAME every 10 s.
//
// Configuration (environment, or the run configuration at CONFIG_URL):
//
//	NAME          api | orders | inventory | payments
//	DOWNSTREAM    the services this one calls, comma-separated base URLs
//	              (none for the bottom services)
//	MODE          envoy | aquifer | envoy-idem | envoy-queue (bottom services)
//	REDIS_URL     redis://host:port (envoy-idem and envoy-queue)
//	FRONT_URL     the front door's base URL (bottom services)
//	BACKEND_URL   the backend's /work URL, for Aquifer's job (aquifer mode)
//	ATTEMPTS      tries per call (default 3)
//	RETRY_DELAYS  waits between tries (default "50ms,100ms")
//	TIMEOUT       per-try timeout (default 5s)
//	START_AT      Unix seconds the run starts, for the report's intervals
//	REPORT_URL    where to POST the counts; svc-NAME is appended
//	LISTEN        address (default :8080)
//	CONFIG_URL    as in the client: wait for the run's settings there
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type svc struct {
	name, mode, front, backendURL string
	downstream                    []string
	attempts                      int
	delays                        []time.Duration
	timeout                       time.Duration
	http                          *http.Client
	rdb                           *redis.Client

	// Calls waiting for a job's result, woken by its done:<job> message.
	wmu     sync.Mutex
	waiting map[string][]chan struct{}

	mu       sync.Mutex
	counts   counts
	interval map[string]int
}

// counts is what the service saw, over the whole run.
type counts struct {
	Received      int            `json:"received"`
	Answered      int            `json:"answered_ok"`
	Failed        int            `json:"answered_503"`
	CallerGone    int            `json:"caller_gone"`
	Sent          int            `json:"sent"`
	FrontDoor     int            `json:"front_door"`
	FromStore     int            `json:"answered_from_store"`
	SentRetries   int            `json:"sent_retries"`
	SentFailures  map[string]int `json:"sent_failures"`
	ReceivedByTen map[string]int `json:"received_by_tenant"`
	SentByTen     map[string]int `json:"sent_by_tenant"`
}

func main() {
	if u := os.Getenv("CONFIG_URL"); u != "" {
		loadConfig(u)
	}
	s := &svc{
		name:       envStr("NAME", "service"),
		mode:       envStr("MODE", "envoy"),
		front:      strings.TrimRight(os.Getenv("FRONT_URL"), "/"),
		backendURL: os.Getenv("BACKEND_URL"),
		attempts:   envInt("ATTEMPTS", 3),
		timeout:    envDur("TIMEOUT", 5*time.Second),
		http:       &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1024}},
		counts:     counts{SentFailures: map[string]int{}, ReceivedByTen: map[string]int{}, SentByTen: map[string]int{}},
		interval:   map[string]int{},
		waiting:    map[string][]chan struct{}{},
	}
	for _, u := range strings.Split(os.Getenv("DOWNSTREAM"), ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			s.downstream = append(s.downstream, u)
		}
	}
	for _, d := range strings.Split(envStr("RETRY_DELAYS", "50ms,100ms"), ",") {
		v, err := time.ParseDuration(strings.TrimSpace(d))
		if err != nil {
			log.Fatalf("RETRY_DELAYS: %v", err)
		}
		s.delays = append(s.delays, v)
	}
	if len(s.downstream) == 0 {
		switch s.mode {
		case "envoy", "aquifer":
		case "envoy-idem", "envoy-queue":
			opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
			if err != nil {
				log.Fatalf("REDIS_URL: %v", err)
			}
			// Every request in flight may hold a connection while it waits.
			opt.PoolSize = 512
			s.rdb = redis.NewClient(opt)
			go s.listen()
		default:
			log.Fatalf("MODE must be envoy, aquifer, envoy-idem or envoy-queue, got %q", s.mode)
		}
	}
	if u := os.Getenv("REPORT_URL"); u != "" {
		go s.report(strings.TrimRight(u, "/") + "/svc-" + s.name)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /req", s.req)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Printf("%s: %d downstream, mode %s, %d attempts, timeout %s", s.name, len(s.downstream), s.mode, s.attempts, s.timeout)
	log.Fatal(http.ListenAndServe(envStr("LISTEN", ":8080"), mux))
}

func (s *svc) req(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	tenant, id := r.Header.Get("X-Tenant"), r.Header.Get("X-Job-Id")
	s.mu.Lock()
	s.counts.Received++
	s.counts.ReceivedByTen[tenant]++
	s.interval["received"]++
	s.mu.Unlock()

	var err error
	if len(s.downstream) == 0 {
		job := id + "-" + s.name
		err = s.call(r.Context(), tenant, id, func(ctx context.Context) error { return s.bottom(ctx, tenant, job) })
	} else {
		errs := make([]error, len(s.downstream))
		var wg sync.WaitGroup
		for i, u := range s.downstream {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = s.call(r.Context(), tenant, id, func(ctx context.Context) error { return s.next(ctx, u, tenant, id) })
			}()
		}
		wg.Wait()
		err = errors.Join(errs...)
	}

	s.mu.Lock()
	switch {
	case r.Context().Err() != nil:
		s.counts.CallerGone++
	case err != nil:
		s.counts.Failed++
	default:
		s.counts.Answered++
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte(`{"ok":true}`))
}

// call tries one downstream call up to ATTEMPTS times, RETRY_DELAYS apart.
func (s *svc) call(ctx context.Context, tenant, id string, try func(context.Context) error) error {
	var err error
	for attempt := 0; attempt < s.attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.delays[min(attempt-1, len(s.delays)-1)]):
			}
		}
		s.mu.Lock()
		s.counts.Sent++
		s.counts.SentByTen[tenant]++
		s.interval["sent"]++
		if attempt > 0 {
			s.counts.SentRetries++
		}
		s.mu.Unlock()
		actx, cancel := context.WithTimeout(ctx, s.timeout)
		err = try(actx)
		cancel()
		if err == nil {
			return nil
		}
		why := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			why = "timeout"
		}
		s.mu.Lock()
		s.counts.SentFailures[why]++
		s.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return err
}

// next calls another service in the chain.
func (s *svc) next(ctx context.Context, base, tenant, id string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/req", nil)
	req.Header.Set("X-Tenant", tenant)
	req.Header.Set("X-Job-Id", id)
	resp, err := s.http.Do(req)
	if err != nil {
		return errOf(ctx, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http_%d", resp.StatusCode)
	}
	return nil
}

// bottom runs this service's backend job through the front door.
func (s *svc) bottom(ctx context.Context, tenant, job string) error {
	switch s.mode {
	case "envoy-idem":
		return s.idem(ctx, tenant, job)
	case "envoy-queue":
		return s.queue(ctx, tenant, job)
	case "envoy":
		return s.work(ctx, tenant, job)
	}
	s.mu.Lock()
	s.counts.FrontDoor++
	s.mu.Unlock()
	body, _ := json.Marshal(map[string]any{
		"user_id":        tenant,
		"idempotent_key": job,
		"url":            s.backendURL,
		"method":         http.MethodPost,
		"headers":        map[string]string{"X-Tenant": tenant, "X-Job-Id": job},
		"max_retries":    -1,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.front+"/proxy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", tenant)
	resp, err := s.http.Do(req)
	if err != nil {
		return errOf(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("http_%d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The backend's own answer (Aquifer ran it straight away), or a
		// repeat of a job already finished.
		var out struct {
			Status    string `json:"status"`
			Duplicate bool   `json:"duplicate"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
		if out.Duplicate && out.Status != "completed" {
			return fmt.Errorf("aquifer_%s", out.Status)
		}
		return nil
	}
	// Held in Aquifer's queue: wait on its events for the outcome.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:") && (event == "completed" || event == "failed"):
			var out struct {
				ResponseStatus int `json:"response_status"`
			}
			json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &out)
			if event == "completed" && (out.ResponseStatus == 0 || out.ResponseStatus == http.StatusOK) {
				return nil
			}
			return fmt.Errorf("aquifer_%s", event)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("stream_ended")
}

// work runs the job through Envoy's /work.
func (s *svc) work(ctx context.Context, tenant, job string) error {
	s.mu.Lock()
	s.counts.FrontDoor++
	s.mu.Unlock()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.front+"/work", nil)
	req.Header.Set("X-Tenant", tenant)
	req.Header.Set("X-Job-Id", job)
	resp, err := s.http.Do(req)
	if err != nil {
		return errOf(ctx, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http_%d", resp.StatusCode)
	}
	return nil
}

// idem runs the job through Envoy at most once at a time: a finished job is
// answered from its stored result, and while one call holds the job's lock,
// the others wait for its result instead of calling the backend. A refused
// or failed call releases the lock, so the next try runs the job.
func (s *svc) idem(ctx context.Context, tenant, job string) error {
	done := s.wait(job)
	defer s.unwait(job, done)
	if ok, err := s.finished(ctx, job); err != nil || ok {
		return err
	}
	got, err := s.rdb.SetNX(ctx, "lock:"+job, s.name, 30*time.Second).Result()
	if err != nil {
		return errOf(ctx, err)
	}
	if !got {
		return s.await(ctx, job, done)
	}
	if err := s.work(ctx, tenant, job); err != nil {
		s.rdb.Del(context.WithoutCancel(ctx), "lock:"+job)
		return err
	}
	return s.store(context.WithoutCancel(ctx), job)
}

// queue puts the job on its tenant's list once and waits for its result;
// cmd/dispatcher runs it.
func (s *svc) queue(ctx context.Context, tenant, job string) error {
	done := s.wait(job)
	defer s.unwait(job, done)
	if ok, err := s.finished(ctx, job); err != nil || ok {
		return err
	}
	fresh, err := s.rdb.SetNX(ctx, "queued:"+job, tenant, time.Hour).Result()
	if err != nil {
		return errOf(ctx, err)
	}
	if fresh {
		_, err = s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.RPush(ctx, "q:"+tenant, job)
			p.SAdd(ctx, "tenants", tenant)
			return nil
		})
		if err != nil {
			s.rdb.Del(context.WithoutCancel(ctx), "queued:"+job)
			return errOf(ctx, err)
		}
	}
	return s.await(ctx, job, done)
}

// finished reports whether the job's result is already stored.
func (s *svc) finished(ctx context.Context, job string) (bool, error) {
	n, err := s.rdb.Exists(ctx, "result:"+job).Result()
	if err != nil {
		return false, errOf(ctx, err)
	}
	if n > 0 {
		s.mu.Lock()
		s.counts.FromStore++
		s.mu.Unlock()
	}
	return n > 0, nil
}

// store records the job's result and wakes everyone waiting for it.
func (s *svc) store(ctx context.Context, job string) error {
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, "result:"+job, "ok", time.Hour)
		p.Publish(ctx, "done:"+job, "ok")
		return nil
	})
	return err
}

// await waits for the job's result, or for this try to end.
func (s *svc) await(ctx context.Context, job string, done chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// The result may have landed between the check and the wait.
		if n, _ := s.rdb.Exists(context.WithoutCancel(ctx), "result:"+job).Result(); n > 0 {
			return nil
		}
		return ctx.Err()
	}
}

// wait registers a call waiting for job; register before checking for the
// result, so a result stored in between still wakes it.
func (s *svc) wait(job string) chan struct{} {
	ch := make(chan struct{})
	s.wmu.Lock()
	s.waiting[job] = append(s.waiting[job], ch)
	s.wmu.Unlock()
	return ch
}

func (s *svc) unwait(job string, ch chan struct{}) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	list := s.waiting[job]
	for i, c := range list {
		if c == ch {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(s.waiting, job)
	} else {
		s.waiting[job] = list
	}
}

// listen wakes the calls waiting for each job whose result is published: one
// subscription for this service's jobs, not one per call.
func (s *svc) listen() {
	ps := s.rdb.PSubscribe(context.Background(), "done:*-"+s.name)
	for msg := range ps.Channel() {
		job := strings.TrimPrefix(msg.Channel, "done:")
		s.wmu.Lock()
		for _, ch := range s.waiting[job] {
			close(ch)
		}
		delete(s.waiting, job)
		s.wmu.Unlock()
	}
}

// errOf names a failed request: a timeout or cancellation, or a network error.
func errOf(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("error")
}

// report POSTs the counts every 10 s, with the requests received and sent in
// each 10 s interval since the run started.
func (s *svc) report(url string) {
	start := time.Now()
	if v, err := strconv.ParseInt(os.Getenv("START_AT"), 10, 64); err == nil {
		start = time.Unix(v, 0)
	}
	time.Sleep(time.Until(start))
	intervals := []map[string]any{}
	for range time.Tick(10 * time.Second) {
		s.mu.Lock()
		intervals = append(intervals, map[string]any{"t": int(time.Since(start).Seconds()), "received": s.interval["received"], "sent": s.interval["sent"]})
		s.interval = map[string]int{}
		out := map[string]any{"name": s.name, "mode": s.mode, "counts": s.counts, "intervals": intervals}
		s.mu.Unlock()
		if s.rdb != nil {
			// The shared store's own counters: calls, CPU and memory.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			out["redis_info"], _ = s.rdb.Info(ctx, "stats", "commandstats", "cpu", "memory").Result()
			cancel()
		}
		s.mu.Lock()
		raw, _ := json.Marshal(out)
		s.mu.Unlock()
		if resp, err := control.Post(url, "application/json", bytes.NewReader(raw)); err == nil {
			resp.Body.Close()
		}
	}
}

// control carries the run's own traffic, bounded so a stuck peer can't hang it.
var control = &http.Client{Timeout: 10 * time.Second}

// loadConfig polls url until it returns the run's settings, then sets them
// in the environment. Settings already in the environment win.
func loadConfig(url string) {
	for {
		resp, err := control.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			var cfg map[string]string
			err = json.NewDecoder(resp.Body).Decode(&cfg)
			resp.Body.Close()
			if err == nil {
				for k, v := range cfg {
					if os.Getenv(k) == "" {
						os.Setenv(k, v)
					}
				}
				return
			}
		} else if err == nil {
			resp.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envDur(name string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envStr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
