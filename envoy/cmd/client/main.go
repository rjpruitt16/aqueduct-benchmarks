// Command client generates load for one region and measures what it costs
// the caller to get work done, in one of two modes:
//
//	envoy    POST each job to Envoy and retry like a well-behaved SDK:
//	         exponential backoff with full jitter, honoring Retry-After.
//	aquifer  hand each job to Aquifer once (retrying only a refused
//	         submission, the same way) and wait for Aquifer's webhook.
//
// Every request the client sends is counted. A share of jobs carries a
// deadline; those count as missed if the answer arrives too late.
//
// Configuration (environment):
//
//	MODE            envoy | aquifer
//	TARGET          Envoy's or Aquifer's base URL
//	BACKEND_URL     the backend's /work URL (aquifer mode)
//	PUBLIC_URL      this client's base URL for Aquifer's webhooks
//	NAME            name for the report, e.g. the region
//	TENANTS         name:rate pairs at 1x load, e.g. "big:20,small-1:1"
//	PHASES          name:duration:multiplier, e.g. "steady:10m:0.5,spike:5m:3"
//	START_AT        Unix seconds to start (all clients share it)
//	DRAIN           how long to wait for outstanding work (default 10m)
//	DEADLINE_SHARE  fraction of jobs with a deadline (default 0.1)
//	DEADLINE        that deadline (default 2s)
//	MAX_ATTEMPTS    attempts per job, or per submission (default 6)
//	BACKOFF_BASE    first backoff (default 200ms)
//	BACKOFF_MAX     backoff cap (default 20s)
//	TIMEOUT         per-attempt timeout (default 10s)
//	REPORT_URL      where to POST the final report
//	LISTEN          webhook listener address (default :8081)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type phase struct {
	name string
	dur  time.Duration
	mult float64
}

type tenant struct {
	name string
	rate float64
}

type job struct {
	id       string
	tenant   string
	phase    string
	created  time.Time
	deadline time.Time // zero: none
	done     chan result
}

type result struct {
	ok  bool
	why string
}

// tally is one tenant's counts and latencies, by phase.
type tally struct {
	Offered        int            `json:"offered"`
	Completed      int            `json:"completed"`
	Failed         map[string]int `json:"failed"`
	Requests       int            `json:"requests"`
	Refused        int            `json:"refused"`
	DeadlineJobs   int            `json:"deadline_jobs"`
	DeadlineMet    int            `json:"deadline_met"`
	DeadlineLate   int            `json:"deadline_late"`
	latency        map[string][]float64
	firstAnswer    map[string][]float64
	LatencyP50     map[string]float64 `json:"latency_p50_ms"`
	LatencyP99     map[string]float64 `json:"latency_p99_ms"`
	FirstAnswerP50 map[string]float64 `json:"first_answer_p50_ms"`
	FirstAnswerP99 map[string]float64 `json:"first_answer_p99_ms"`
}

type client struct {
	mode, target, backendURL, publicURL string
	maxAttempts                         int
	base, cap, timeout                  time.Duration
	http                                *http.Client

	mu       sync.Mutex
	tallies  map[string]*tally
	byAqID   map[string]*job
	early    map[string]result
	byID     map[string]*job
	interval map[string]int
	open     atomic.Int64
}

func main() {
	c := &client{
		mode:        envStr("MODE", "envoy"),
		target:      strings.TrimRight(os.Getenv("TARGET"), "/"),
		backendURL:  os.Getenv("BACKEND_URL"),
		publicURL:   strings.TrimRight(os.Getenv("PUBLIC_URL"), "/"),
		maxAttempts: envInt("MAX_ATTEMPTS", 6),
		base:        envDur("BACKOFF_BASE", 200*time.Millisecond),
		cap:         envDur("BACKOFF_MAX", 20*time.Second),
		timeout:     envDur("TIMEOUT", 10*time.Second),
		tallies:     map[string]*tally{},
		byAqID:      map[string]*job{},
		early:       map[string]result{},
		byID:        map[string]*job{},
		interval:    map[string]int{},
	}
	c.http = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 512, MaxConnsPerHost: 0}}
	if c.mode != "envoy" && c.mode != "aquifer" {
		log.Fatalf("MODE must be envoy or aquifer, got %q", c.mode)
	}
	tenants := parseTenants(os.Getenv("TENANTS"))
	phases := parsePhases(envStr("PHASES", "steady:10m:1"))
	name := envStr("NAME", "client")
	deadlineShare := envFloat("DEADLINE_SHARE", 0.1)
	deadline := envDur("DEADLINE", 2*time.Second)
	for _, t := range tenants {
		c.tallies[t.name] = newTally()
	}

	if c.mode == "aquifer" {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /done", c.webhook)
		go func() { log.Fatal(http.ListenAndServe(envStr("LISTEN", ":8081"), mux)) }()
	}

	start := time.Now()
	if s := os.Getenv("START_AT"); s != "" {
		sec, _ := strconv.ParseInt(s, 10, 64)
		start = time.Unix(sec, 0)
	}
	time.Sleep(time.Until(start))
	log.Printf("%s: %s mode, %d tenants, started", name, c.mode, len(tenants))

	intervals := []map[string]any{}
	stopSampling := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-tick.C:
				c.mu.Lock()
				done := c.interval
				c.interval = map[string]int{}
				c.mu.Unlock()
				intervals = append(intervals, map[string]any{"t": int(time.Since(start).Seconds()), "open": c.open.Load(), "completed": done})
			}
		}
	}()

	var wg sync.WaitGroup
	seq := atomic.Int64{}
	for _, p := range phases {
		log.Printf("phase %s: %.2fx for %s", p.name, p.mult, p.dur)
		end := time.Now().Add(p.dur)
		var pw sync.WaitGroup
		for _, t := range tenants {
			pw.Add(1)
			go func(t tenant) {
				defer pw.Done()
				rate := t.rate * p.mult
				if rate <= 0 {
					time.Sleep(time.Until(end))
					return
				}
				next := time.Now()
				for {
					// Poisson arrivals at this tenant's rate.
					next = next.Add(time.Duration(rand.ExpFloat64() / rate * float64(time.Second)))
					if next.After(end) {
						time.Sleep(time.Until(end))
						return
					}
					time.Sleep(time.Until(next))
					j := &job{id: fmt.Sprintf("%s-%s-%d", name, t.name, seq.Add(1)), tenant: t.name, phase: p.name, created: time.Now(), done: make(chan result, 1)}
					if rand.Float64() < deadlineShare {
						j.deadline = j.created.Add(deadline)
					}
					c.mu.Lock()
					c.tallies[t.name].Offered++
					if !j.deadline.IsZero() {
						c.tallies[t.name].DeadlineJobs++
					}
					c.mu.Unlock()
					wg.Add(1)
					c.open.Add(1)
					go func() {
						defer wg.Done()
						defer c.open.Add(-1)
						c.run(j)
					}()
				}
			}(t)
		}
		pw.Wait()
	}

	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(envDur("DRAIN", 10*time.Minute)):
		log.Printf("drain timed out with %d jobs open", c.open.Load())
	}
	close(stopSampling)
	<-sampled

	c.mu.Lock()
	report := map[string]any{"name": name, "mode": c.mode, "open_at_end": c.open.Load(), "tenants": c.tallies, "intervals": intervals}
	for _, t := range c.tallies {
		t.summarize()
	}
	raw, _ := json.Marshal(report)
	c.mu.Unlock()
	if u := os.Getenv("REPORT_URL"); u != "" {
		for i := 0; i < 10; i++ {
			resp, err := http.Post(u, "application/json", bytes.NewReader(raw))
			if err == nil && resp.StatusCode < 300 {
				resp.Body.Close()
				break
			}
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(3 * time.Second)
		}
	}
	os.Stdout.Write(raw)
	log.Printf("%s: done", name)
}

// run carries one job to an outcome and records it.
func (c *client) run(j *job) {
	var res result
	if c.mode == "envoy" {
		res = c.viaEnvoy(j)
	} else {
		res = c.viaAquifer(j)
	}
	elapsed := float64(time.Since(j.created).Milliseconds())
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tallies[j.tenant]
	if res.ok {
		t.Completed++
		c.interval[j.tenant]++
		t.latency[j.phase] = append(t.latency[j.phase], elapsed)
		if !j.deadline.IsZero() {
			if time.Now().After(j.deadline) {
				t.DeadlineLate++
			} else {
				t.DeadlineMet++
			}
		}
		return
	}
	t.Failed[res.why]++
}

func (c *client) viaEnvoy(j *job) result {
	for attempt := 1; ; attempt++ {
		timeout := c.timeout
		if !j.deadline.IsZero() {
			timeout = min(timeout, time.Until(j.deadline))
			if timeout <= 0 {
				return result{why: "deadline"}
			}
		}
		status, retryAfter, err := c.post(j, c.target+"/work", nil, timeout)
		if attempt == 1 {
			c.firstAnswer(j)
		}
		if err == nil && status == http.StatusOK {
			return result{ok: true}
		}
		why := "error"
		if err == nil {
			why = "http_" + strconv.Itoa(status)
			if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
				c.refused(j)
			}
		} else if errorsIsTimeout(err) {
			why = "timeout"
		}
		if attempt >= c.maxAttempts {
			return result{why: why}
		}
		if !c.backoff(j, attempt, retryAfter) {
			return result{why: "deadline"}
		}
	}
}

func (c *client) viaAquifer(j *job) result {
	body := map[string]any{
		"user_id":        j.tenant,
		"idempotent_key": j.id,
		"url":            c.backendURL,
		"method":         http.MethodPost,
		"headers":        map[string]string{"X-Tenant": j.tenant, "X-Job-Id": j.id},
		"webhook_url":    c.publicURL + "/done",
		"max_retries":    -1,
	}
	if !j.deadline.IsZero() {
		body["execute_before"] = j.deadline.UnixMilli()
	}
	c.mu.Lock()
	c.byID[j.id] = j
	c.mu.Unlock()
	for attempt := 1; ; attempt++ {
		status, retryAfter, aqID, err := c.submit(body)
		if attempt == 1 {
			c.firstAnswer(j)
		}
		if err == nil && (status == http.StatusCreated || status == http.StatusOK) {
			c.mu.Lock()
			c.byAqID[aqID] = j
			early, ok := c.early[aqID]
			delete(c.early, aqID)
			c.mu.Unlock()
			if ok {
				return early
			}
			select {
			case res := <-j.done:
				return res
			case <-time.After(2 * time.Hour):
				return result{why: "no_webhook"}
			}
		}
		why := "error"
		if err == nil {
			why = "http_" + strconv.Itoa(status)
			if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
				c.refused(j)
			}
		}
		if attempt >= c.maxAttempts {
			return result{why: why}
		}
		if !c.backoff(j, attempt, retryAfter) {
			return result{why: "deadline"}
		}
	}
}

// webhook receives Aquifer's report that a job finished or gave up.
func (c *client) webhook(w http.ResponseWriter, r *http.Request) {
	var p struct {
		JobID          string `json:"job_id"`
		Status         string `json:"status"`
		ResponseStatus int    `json:"response_status"`
		Body           string `json:"body"`
		Reason         string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	res := result{ok: p.Status == "completed" && p.ResponseStatus == http.StatusOK}
	if !res.ok {
		res.why = "aquifer_" + p.Status
		if p.Reason != "" {
			res.why += ":" + strings.SplitN(p.Reason, " ", 2)[0]
		}
	}
	c.mu.Lock()
	j := c.byAqID[p.JobID]
	if j == nil {
		// The backend echoes our job ID; use it when the webhook beats
		// the submission's response.
		var echo struct {
			Job string `json:"job"`
		}
		json.Unmarshal([]byte(p.Body), &echo)
		j = c.byID[echo.Job]
	}
	if j == nil {
		c.early[p.JobID] = res
		c.mu.Unlock()
		return
	}
	delete(c.byAqID, p.JobID)
	delete(c.byID, j.id)
	c.mu.Unlock()
	select {
	case j.done <- res:
	default:
	}
}

func (c *client) post(j *job, url string, body []byte, timeout time.Duration) (int, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("X-Tenant", j.tenant)
	req.Header.Set("X-Job-Id", j.id)
	c.request(j.tenant)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, retryAfter(resp.Header.Get("Retry-After")), nil
}

func (c *client) submit(body map[string]any) (int, time.Duration, string, error) {
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.target+"/jobs", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	c.request(body["user_id"].(string))
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, 0, "", err
	}
	defer resp.Body.Close()
	var out struct {
		JobID string `json:"job_id"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	return resp.StatusCode, retryAfter(resp.Header.Get("Retry-After")), out.JobID, nil
}

// backoff sleeps before the next attempt: full jitter on an exponential
// window, but never less than Retry-After. It returns false when the job's
// deadline would pass first.
func (c *client) backoff(j *job, attempt int, retryAfter time.Duration) bool {
	window := min(c.cap, time.Duration(float64(c.base)*math.Pow(2, float64(attempt-1))))
	d := max(time.Duration(rand.Float64()*float64(window)), retryAfter)
	if !j.deadline.IsZero() && time.Now().Add(d).After(j.deadline) {
		return false
	}
	time.Sleep(d)
	return true
}

func (c *client) request(tenant string) {
	c.mu.Lock()
	c.tallies[tenant].Requests++
	c.mu.Unlock()
}

func (c *client) refused(j *job) {
	c.mu.Lock()
	c.tallies[j.tenant].Refused++
	c.mu.Unlock()
}

func (c *client) firstAnswer(j *job) {
	ms := float64(time.Since(j.created).Milliseconds())
	c.mu.Lock()
	t := c.tallies[j.tenant]
	t.firstAnswer[j.phase] = append(t.firstAnswer[j.phase], ms)
	c.mu.Unlock()
}

func newTally() *tally {
	return &tally{Failed: map[string]int{}, latency: map[string][]float64{}, firstAnswer: map[string][]float64{}}
}

func (t *tally) summarize() {
	t.LatencyP50, t.LatencyP99 = percentiles(t.latency)
	t.FirstAnswerP50, t.FirstAnswerP99 = percentiles(t.firstAnswer)
}

func percentiles(by map[string][]float64) (p50, p99 map[string]float64) {
	p50, p99 = map[string]float64{}, map[string]float64{}
	all := []float64{}
	for phase, xs := range by {
		p50[phase], p99[phase] = pct(xs, 0.5), pct(xs, 0.99)
		all = append(all, xs...)
	}
	p50["all"], p99["all"] = pct(all, 0.5), pct(all, 0.99)
	return p50, p99
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[min(len(s)-1, int(p*float64(len(s))))]
}

func retryAfter(v string) time.Duration {
	if sec, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return 0
}

func errorsIsTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	t, ok := err.(timeout)
	return ok && t.Timeout() || strings.Contains(err.Error(), "deadline exceeded")
}

func parseTenants(spec string) []tenant {
	var out []tenant
	for _, part := range strings.Split(spec, ",") {
		name, rate, ok := strings.Cut(strings.TrimSpace(part), ":")
		r, err := strconv.ParseFloat(rate, 64)
		if !ok || err != nil {
			log.Fatalf("TENANTS: bad entry %q", part)
		}
		out = append(out, tenant{name, r})
	}
	return out
}

func parsePhases(spec string) []phase {
	var out []phase
	for _, part := range strings.Split(spec, ",") {
		f := strings.Split(strings.TrimSpace(part), ":")
		if len(f) != 3 {
			log.Fatalf("PHASES: bad entry %q", part)
		}
		d, err1 := time.ParseDuration(f[1])
		m, err2 := strconv.ParseFloat(f[2], 64)
		if err1 != nil || err2 != nil {
			log.Fatalf("PHASES: bad entry %q", part)
		}
		out = append(out, phase{f[0], d, m})
	}
	return out
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil {
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
