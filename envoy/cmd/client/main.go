// Command client generates load for one region and measures what it costs
// the caller to get work done, in one of two modes:
//
//	envoy    POST each job to Envoy and retry like a well-behaved SDK:
//	         exponential backoff with full jitter, honoring Retry-After.
//	aquifer  hand each job to Aquifer once (retrying only a refused
//	         submission, the same way) and wait for Aquifer's webhook;
//	         with CALL=proxy, call Aquifer's /proxy instead and retry it
//	         exactly as the envoy mode retries /work: a retry with the same
//	         idempotency key waits on the job Aquifer already holds.
//
// Every request the client sends is counted. A share of jobs carries a
// 2 s target; those count as late if the answer arrives after it. The job
// still runs to its normal end either way (unless ENFORCE_DEADLINE=1).
//
// Three workloads:
//
//	jobs    (default) independent jobs arriving at each tenant's rate,
//	        whatever happened to the earlier ones.
//	chains  each tenant works in lanes. A lane runs a chain of four jobs,
//	        A, then B and C together, then D, and starts the next chain
//	        CHAIN_EVERY after the last one started, or as soon as it
//	        finishes if it ran longer. A step that is refused is retried
//	        until it succeeds, so a slow step holds up its chain and its
//	        lane. Lanes per tenant: rate x phase multiplier x CHAIN_EVERY
//	        / 4, rounded up, so with no contention the offered load is the
//	        same as in the jobs workload.
//	wild    each tenant runs WORKERS workers (the tenant's fourth field).
//	        A worker sends one request to the api service (TARGET/req),
//	        retries it until it succeeds, and only then sends the next; it
//	        starts requests at random at its share of the tenant's rate, or
//	        straight after the last one if that took longer. The front door
//	        sits further down, behind the services (cmd/service), so MODE
//	        doesn't change what the client does. During BURSTS the load is
//	        at least BURST_MULT.
//
// Configuration (environment):
//
//	MODE            envoy | aquifer | both (aquifer behind an Envoy edge:
//	                the client behaves as in aquifer mode)
//	TARGET          Envoy's or Aquifer's base URL
//	BACKEND_URL     the backend's /work URL (aquifer mode)
//	POOL            send to this Aquifer pool instead of BACKEND_URL (aquifer mode)
//	PUBLIC_URL      this client's base URL for Aquifer's webhooks
//	NAME            name for the report, e.g. the region
//	TENANTS         name:rate[:style[:workers]] at 1x load, e.g.
//	                "big:20,small-1:1:aggressive". Styles: polite (default)
//	                backs off exponentially with full jitter and honors
//	                Retry-After; nojitter backs off exponentially without
//	                jitter, so clients that fail together retry together;
//	                aggressive retries every AGGRESSIVE_DELAY and ignores
//	                Retry-After; stack (wild only) is an HTTP library's 3
//	                quick tries inside an SDK's 3 tries, 1 s and 2 s apart,
//	                inside the app's own polite loop. Workers: wild only
//	                (default 10).
//	WORKLOAD        jobs | chains | wild (default jobs)
//	CHAIN_EVERY     how often a lane starts a chain (default 4s)
//	AGGRESSIVE_DELAY  an aggressive tenant's retry delay (default 100ms)
//	PHASES          name:duration:multiplier, e.g. "steady:10m:0.5,spike:5m:3"
//	BURSTS          wild only: start:duration after START_AT, e.g. "6m:30s,8m40s:15s"
//	BURST_MULT      the load during a burst, at least (default 4)
//	START_AT        Unix seconds to start (all clients share it)
//	DRAIN           how long to wait for outstanding work (default 10m)
//	DEADLINE_SHARE  fraction of jobs with a deadline (default 0.1)
//	DEADLINE        that deadline (default 2s)
//	ENFORCE_DEADLINE  1 to give up on a job at its deadline (Aquifer's
//	                execute_before; no Envoy retry past it). Off by default:
//	                the deadline is only measured. The env series ran with it on.
//	MAX_ATTEMPTS    attempts per job, or per submission (default 6; 0 retries
//	                until it succeeds)
//	BACKOFF_BASE    first backoff (default 200ms)
//	BACKOFF_MAX     backoff cap (default 20s)
//	TIMEOUT         per-attempt timeout (default 10s)
//	PATIENCE        jobs workload: the caller gives up this long after the
//	                job was created (default 0: never). No attempt runs
//	                past it, and no retry starts after it. Work the front
//	                door already holds may still run; the backend counts it.
//	CALL            aquifer mode, jobs workload: "proxy" to call /proxy (see
//	                above), or "poll" (see HOLD), instead of submitting to
//	                /jobs and waiting for a webhook
//	HOLD            1 to call /proxy as Aquifer's own client would (CALL=proxy):
//	                hold the stream for up to a minute at a time and reconnect
//	                at once with the same key; when Aquifer answers 202 or
//	                ends the stream with a poll event (too far back to hold,
//	                or no stream to spare), come back when it says; back off
//	                (up to 10s, jittered) only after a refusal or a network
//	                error, honoring Retry-After. PATIENCE still applies.
//	                CALL=poll instead submits to /jobs once and polls GET
//	                /jobs/{id} when Aquifer's Retry-After says, with the same
//	                backoff after a refusal.
//	JOB_LOG         1 to add every job to the report as [id, created ms,
//	                ended ms, outcome] (Unix ms; outcome "ok" or why it failed)
//	NO_KEY          1 to send a new idempotency key on every attempt, as a
//	                caller with no stable job ID would (CALL=proxy); the
//	                backend still sees the real job ID, so it counts repeats
//	REPORT_URL      where to POST the final report; NAME is appended
//	LISTEN          webhook listener address (default :8081)
//	CONFIG_URL      if set, wait for a JSON object of these settings there
//	                and apply it before starting (so a run can start clients
//	                before it knows the shared settings)
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
	name    string
	rate    float64
	style   string
	workers int
}

type job struct {
	id       string
	tenant   string
	phase    string
	created  time.Time
	deadline time.Time // zero: none
	attempts int
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
	Timeouts       int            `json:"timeouts"`
	DeadlineJobs   int            `json:"deadline_jobs"`
	DeadlineMet    int            `json:"deadline_met"`
	DeadlineLate   int            `json:"deadline_late"`
	MostAttempts   int            `json:"most_attempts"`
	BackoffSeconds float64        `json:"backoff_seconds"`
	attempts       map[string][]float64
	AttemptsP50    map[string]float64 `json:"attempts_p50"`
	AttemptsP99    map[string]float64 `json:"attempts_p99"`
	Chains         int                `json:"chains_completed"`
	chain          map[string][]float64
	ChainP50       map[string]float64 `json:"chain_p50_ms,omitempty"`
	ChainP99       map[string]float64 `json:"chain_p99_ms,omitempty"`
	latency        map[string][]float64
	firstAnswer    map[string][]float64
	LatencyP50     map[string]float64 `json:"latency_p50_ms"`
	LatencyP99     map[string]float64 `json:"latency_p99_ms"`
	FirstAnswerP50 map[string]float64 `json:"first_answer_p50_ms"`
	FirstAnswerP99 map[string]float64 `json:"first_answer_p99_ms"`
}

type client struct {
	mode, target, backendURL, publicURL string
	workload                            string
	enforce, noKey, hold, jobLog        bool
	call                                string
	patience                            time.Duration
	pool                                string
	maxAttempts                         int
	base, cap, timeout, aggressive      time.Duration
	styles                              map[string]string
	http                                *http.Client

	mu       sync.Mutex
	tallies  map[string]*tally
	byAqID   map[string]*job
	early    map[string]result
	byID     map[string]*job
	interval map[string]int
	sent     map[string]int
	jobs     [][4]any
	open     atomic.Int64
}

func main() {
	if u := os.Getenv("CONFIG_URL"); u != "" {
		loadConfig(u)
	}
	if os.Getenv("PUBLIC_URL") == "" && os.Getenv("FLY_MACHINE_ID") != "" {
		os.Setenv("PUBLIC_URL", fmt.Sprintf("http://%s.vm.%s.internal:8081", os.Getenv("FLY_MACHINE_ID"), os.Getenv("FLY_APP_NAME")))
	}
	c := &client{
		mode:        envStr("MODE", "envoy"),
		target:      strings.TrimRight(os.Getenv("TARGET"), "/"),
		backendURL:  os.Getenv("BACKEND_URL"),
		pool:        os.Getenv("POOL"),
		enforce:     os.Getenv("ENFORCE_DEADLINE") == "1",
		noKey:       os.Getenv("NO_KEY") == "1",
		hold:        os.Getenv("HOLD") == "1",
		jobLog:      os.Getenv("JOB_LOG") == "1",
		call:        os.Getenv("CALL"),
		patience:    envDur("PATIENCE", 0),
		publicURL:   strings.TrimRight(os.Getenv("PUBLIC_URL"), "/"),
		maxAttempts: envInt("MAX_ATTEMPTS", 6),
		base:        envDur("BACKOFF_BASE", 200*time.Millisecond),
		cap:         envDur("BACKOFF_MAX", 20*time.Second),
		timeout:     envDur("TIMEOUT", 10*time.Second),
		aggressive:  envDur("AGGRESSIVE_DELAY", 100*time.Millisecond),
		styles:      map[string]string{},
		tallies:     map[string]*tally{},
		byAqID:      map[string]*job{},
		early:       map[string]result{},
		byID:        map[string]*job{},
		interval:    map[string]int{},
		sent:        map[string]int{},
	}
	c.http = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 512, MaxConnsPerHost: 0}}
	if c.mode == "both" {
		c.mode = "aquifer"
	}
	// The do-it-yourself setups only change the bottom services (wild).
	// envoy-max is Envoy with its limits sized for the capacity after scaling.
	if c.mode == "envoy-idem" || c.mode == "envoy-queue" || c.mode == "envoy-max" {
		c.mode = "envoy"
	}
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
		c.styles[t.name] = t.style
	}
	workload := envStr("WORKLOAD", "jobs")
	if workload != "jobs" && workload != "chains" && workload != "wild" {
		log.Fatalf("WORKLOAD must be jobs, chains or wild, got %q", workload)
	}
	c.workload = workload

	if c.mode == "aquifer" && workload != "wild" {
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
				done, sent := c.interval, c.sent
				c.interval, c.sent = map[string]int{}, map[string]int{}
				c.mu.Unlock()
				intervals = append(intervals, map[string]any{"t": int(time.Since(start).Seconds()), "open": c.open.Load(), "completed": done, "requests": sent})
			}
		}
	}()

	var wg sync.WaitGroup
	seq := atomic.Int64{}
	if workload == "chains" {
		c.chains(name, tenants, phases, envDur("CHAIN_EVERY", 4*time.Second), &seq, &wg)
	}
	if workload == "wild" {
		c.wild(name, tenants, phases, parseBursts(os.Getenv("BURSTS")), envFloat("BURST_MULT", 4), &seq, &wg)
	}
	for _, p := range phases {
		if workload != "jobs" {
			break
		}
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
	if c.jobLog {
		report["jobs"] = c.jobs
	}
	for _, t := range c.tallies {
		t.summarize()
	}
	raw, _ := json.Marshal(report)
	c.mu.Unlock()
	if u := os.Getenv("REPORT_URL"); u != "" {
		u = strings.TrimRight(u, "/") + "/" + name
		sent := false
		for i := 0; i < 10 && !sent; i++ {
			resp, err := control.Post(u, "application/json", bytes.NewReader(raw))
			if err == nil {
				sent = resp.StatusCode < 300
				resp.Body.Close()
			}
			if !sent {
				time.Sleep(3 * time.Second)
			}
		}
		if !sent {
			log.Printf("%s: could not deliver the report to %s", name, u)
		}
	}
	os.Stdout.Write(raw)
	log.Printf("%s: done", name)
}

// control carries the run's own traffic (settings, the report), separate
// from the measured requests and bounded so a stuck backend can't hang it.
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

// chains starts each tenant's lanes and returns once the last phase is over;
// wg tracks the lanes still running.
func (c *client) chains(name string, tenants []tenant, phases []phase, every time.Duration, seq *atomic.Int64, wg *sync.WaitGroup) {
	type window struct {
		p          phase
		start, end time.Time
		lanes      map[string]int
	}
	var ws []window
	at := time.Now()
	most := map[string]int{}
	for _, p := range phases {
		w := window{p: p, start: at, end: at.Add(p.dur), lanes: map[string]int{}}
		for _, t := range tenants {
			n := int(math.Ceil(t.rate * p.mult * every.Seconds() / 4))
			w.lanes[t.name] = n
			most[t.name] = max(most[t.name], n)
		}
		ws = append(ws, w)
		at = w.end
	}
	end := at
	phaseAt := func(now time.Time) *window {
		for i := range ws {
			if now.Before(ws[i].end) {
				return &ws[i]
			}
		}
		return nil
	}
	for _, t := range tenants {
		for lane := 0; lane < most[t.name]; lane++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				next := time.Now()
				for {
					time.Sleep(time.Until(next))
					now := time.Now()
					w := phaseAt(now)
					if w == nil {
						return
					}
					if lane >= w.lanes[t.name] {
						// Not needed in this phase: wait for the next one.
						next = w.end
						continue
					}
					next = now.Add(every)
					c.open.Add(1)
					c.chain(name, t.name, w.p.name, now, seq)
					c.open.Add(-1)
				}
			}()
		}
	}
	// Each lane holds wg until it stops: a lane running a chain at the end
	// finishes it first.
	time.Sleep(time.Until(end))
}

// chain runs A, then B and C together, then D, each until it succeeds, and
// records how long the whole chain took.
func (c *client) chain(name, tenant, phase string, start time.Time, seq *atomic.Int64) {
	step := func(s string) {
		j := &job{id: fmt.Sprintf("%s-%s-%d%s", name, tenant, seq.Add(1), s), tenant: tenant, phase: phase, created: time.Now(), done: make(chan result, 1)}
		c.mu.Lock()
		c.tallies[tenant].Offered++
		c.mu.Unlock()
		base := j.id
		for try := 2; !c.run(j); try++ {
			// Only Aquifer can report a failure once retries are unlimited;
			// hand the work over again under a new key.
			j.id = fmt.Sprintf("%s-again%d", base, try)
			j.done = make(chan result, 1)
		}
	}
	step("a")
	var bc sync.WaitGroup
	bc.Add(2)
	go func() { defer bc.Done(); step("b") }()
	go func() { defer bc.Done(); step("c") }()
	bc.Wait()
	step("d")
	elapsed := float64(time.Since(start).Milliseconds())
	c.mu.Lock()
	t := c.tallies[tenant]
	t.Chains++
	t.chain[phase] = append(t.chain[phase], elapsed)
	c.mu.Unlock()
}

type burst struct{ at, dur time.Duration }

// wild starts each tenant's workers and returns once the last phase is over;
// wg tracks the workers still running. A worker's requests start at random
// at its share of the tenant's rate (in a burst, at least burstMult times
// the base rate), or straight after the last one if that took longer.
func (c *client) wild(name string, tenants []tenant, phases []phase, bursts []burst, burstMult float64, seq *atomic.Int64, wg *sync.WaitGroup) {
	start := time.Now()
	type window struct {
		p   phase
		end time.Duration
	}
	var ws []window
	var at time.Duration
	for _, p := range phases {
		at += p.dur
		ws = append(ws, window{p, at})
	}
	end := start.Add(at)
	phaseAt := func(now time.Time) (phase, float64, bool) {
		since := now.Sub(start)
		for _, w := range ws {
			if since < w.end {
				mult := w.p.mult
				for _, b := range bursts {
					if since >= b.at && since < b.at+b.dur {
						mult = max(mult, burstMult)
					}
				}
				return w.p, mult, true
			}
		}
		return phase{}, 0, false
	}
	for _, t := range tenants {
		for range t.workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				next := time.Now()
				for {
					time.Sleep(time.Until(next))
					now := time.Now()
					p, mult, ok := phaseAt(now)
					if !ok {
						return
					}
					if mult <= 0 {
						next = now.Add(time.Second)
						continue
					}
					// The worker's share of the tenant's rate, as random arrivals.
					rate := t.rate * mult / float64(t.workers)
					next = now.Add(time.Duration(rand.ExpFloat64() / rate * float64(time.Second)))
					j := &job{id: fmt.Sprintf("%s-%s-%d", name, t.name, seq.Add(1)), tenant: t.name, phase: p.name, created: now, done: make(chan result, 1)}
					c.mu.Lock()
					c.tallies[t.name].Offered++
					c.mu.Unlock()
					c.open.Add(1)
					c.run(j)
					c.open.Add(-1)
				}
			}()
		}
	}
	time.Sleep(time.Until(end))
}

// viaServices sends a request to the api service until it succeeds, in the
// tenant's retry style. Every try carries the same request ID.
func (c *client) viaServices(j *job) result {
	first := true
	try := func() (bool, time.Duration) {
		status, retryAfter, err := c.post(j, c.target+"/req", nil, c.timeout)
		if first {
			c.firstAnswer(j)
			first = false
		}
		if err == nil && status == http.StatusOK {
			return true, 0
		}
		c.mu.Lock()
		t := c.tallies[j.tenant]
		switch {
		case err != nil && errorsIsTimeout(err):
			t.Timeouts++
		case err == nil && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable):
			t.Refused++
		}
		c.mu.Unlock()
		return false, retryAfter
	}
	for attempt := 1; ; attempt++ {
		var retryAfter time.Duration
		if c.styles[j.tenant] == "stack" {
			// An HTTP library's 3 quick tries, inside an SDK's 3 tries 1 s
			// and 2 s apart, inside the app's own loop below.
			for sdk := 1; sdk <= 3; sdk++ {
				for lib := 1; lib <= 3; lib++ {
					ok, ra := try()
					if ok {
						return result{ok: true}
					}
					retryAfter = ra
					switch {
					case sdk == 3 && lib == 3:
					case lib == 3:
						c.wait(j, time.Duration(sdk)*time.Second)
					default:
						c.wait(j, 50*time.Millisecond)
					}
				}
			}
		} else {
			ok, ra := try()
			if ok {
				return result{ok: true}
			}
			retryAfter = ra
		}
		if c.maxAttempts > 0 && attempt >= c.maxAttempts {
			return result{why: "gave_up"}
		}
		c.backoff(j, attempt, retryAfter)
	}
}

func parseBursts(spec string) []burst {
	var out []burst
	for _, part := range strings.Split(spec, ",") {
		at, d, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			continue
		}
		a, err1 := time.ParseDuration(at)
		l, err2 := time.ParseDuration(d)
		if err1 != nil || err2 != nil {
			log.Fatalf("BURSTS: bad entry %q", part)
		}
		out = append(out, burst{a, l})
	}
	return out
}

// run carries one job to an outcome and records it, reporting whether it
// succeeded.
func (c *client) run(j *job) bool {
	var res result
	switch {
	case c.workload == "wild":
		res = c.viaServices(j)
	case c.mode == "envoy":
		res = c.viaEnvoy(j)
	case c.call == "proxy" && c.hold:
		res = c.holdProxy(j)
	case c.call == "poll":
		res = c.pollAquifer(j)
	case c.call == "proxy":
		res = c.viaProxy(j)
	default:
		res = c.viaAquifer(j)
	}
	elapsed := float64(time.Since(j.created).Milliseconds())
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tallies[j.tenant]
	if c.jobLog {
		outcome := "ok"
		if !res.ok {
			outcome = res.why
		}
		c.jobs = append(c.jobs, [4]any{j.id, j.created.UnixMilli(), time.Now().UnixMilli(), outcome})
	}
	if res.ok {
		t.Completed++
		c.interval[j.tenant]++
		t.latency[j.phase] = append(t.latency[j.phase], elapsed)
		t.attempts[j.phase] = append(t.attempts[j.phase], float64(j.attempts+1))
		t.MostAttempts = max(t.MostAttempts, j.attempts+1)
		if !j.deadline.IsZero() {
			if time.Now().After(j.deadline) {
				t.DeadlineLate++
			} else {
				t.DeadlineMet++
			}
		}
		return true
	}
	t.Failed[res.why]++
	return false
}

func (c *client) viaEnvoy(j *job) result {
	return c.retry(j, func(timeout time.Duration) (int, time.Duration, error) {
		return c.post(j, c.target+"/work", nil, timeout)
	})
}

// viaProxy runs the job through Aquifer's /proxy with the same retries as
// viaEnvoy.
func (c *client) viaProxy(j *job) result {
	return c.retry(j, func(timeout time.Duration) (int, time.Duration, error) {
		return c.proxy(j, timeout)
	})
}

// holdProxy waits on /proxy the way Aquifer's own client would: a retry with
// the same key only reattaches to the job Aquifer holds, so it costs the
// backend nothing and needs no backoff. Each stream is held for up to a
// minute, then reopened at once. When Aquifer has no stream to spare (202)
// the caller comes back when Retry-After says; only a refusal or a network
// error backs off.
func (c *client) holdProxy(j *job) result {
	failures := 0
	for attempt := 1; ; attempt++ {
		left := c.patience - time.Since(j.created)
		if left <= 0 {
			return result{why: "gave_up"}
		}
		status, retryAfter, err := c.proxy(j, min(time.Minute, left))
		if attempt == 1 {
			c.firstAnswer(j)
		}
		switch {
		case err == nil && status == http.StatusOK:
			return result{ok: true}
		case errors.Is(err, context.DeadlineExceeded):
			// Held the whole stream without an outcome: reopen it now.
			failures = 0
			j.attempts++
			continue
		case err == nil && status == http.StatusAccepted:
			failures = 0
			if !c.pace(j, c.told(j, retryAfter)) {
				return result{why: "gave_up"}
			}
			continue
		}
		failures++
		if !c.pace(j, c.refusedWait(j, status, err, failures, retryAfter)) {
			return result{why: "gave_up"}
		}
	}
}

// pollAquifer submits the job to /jobs once and polls GET /jobs/{id} when
// Aquifer's Retry-After says to, with no retry limit: Aquifer's own client
// for a caller that doesn't hold a connection. A missing job is submitted
// again under the same key. Only a refusal or a network error backs off.
func (c *client) pollAquifer(j *job) result {
	jobID := ""
	failures := 0
	for attempt := 1; ; attempt++ {
		left := c.patience - time.Since(j.created)
		if left <= 0 {
			return result{why: "gave_up"}
		}
		var (
			state      string
			status     int
			retryAfter time.Duration
			err        error
		)
		if jobID == "" {
			jobID, status, retryAfter, err = c.submitJob(j, min(c.timeout, left))
		} else {
			state, status, retryAfter, err = c.checkJob(j, jobID, min(c.timeout, left))
		}
		if attempt == 1 {
			c.firstAnswer(j)
		}
		switch {
		case err == nil && state == "ok":
			return result{ok: true}
		case err == nil && state == "failed":
			return result{why: "aquifer_failed"}
		case err == nil && status == http.StatusNotFound:
			jobID = ""
			continue
		case err == nil && (status == http.StatusOK || status == http.StatusCreated || status == http.StatusAccepted):
			failures = 0
			if state == "" && retryAfter == 0 {
				// Just submitted, with no word on when: ask at once where it stands.
				j.attempts++
				continue
			}
			if !c.pace(j, c.told(j, retryAfter)) {
				return result{why: "gave_up"}
			}
			continue
		}
		failures++
		if !c.pace(j, c.refusedWait(j, status, err, failures, retryAfter)) {
			return result{why: "gave_up"}
		}
	}
}

// told is how long to wait when Aquifer says when to come back (it errs
// early); an aggressive caller ignores it.
func (c *client) told(j *job, retryAfter time.Duration) time.Duration {
	if c.styles[j.tenant] == "aggressive" {
		return c.aggressive
	}
	return max(retryAfter, 100*time.Millisecond)
}

// refusedWait backs off after Aquifer refused (429, 503) or didn't answer:
// exponential with full jitter, up to 10 s, or Retry-After if longer.
func (c *client) refusedWait(j *job, status int, err error, failures int, retryAfter time.Duration) time.Duration {
	if err == nil && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) {
		c.refused(j)
	}
	if c.styles[j.tenant] == "aggressive" {
		return c.aggressive
	}
	window := min(10*time.Second, time.Duration(float64(c.base)*math.Pow(2, float64(min(failures-1, 30)))))
	return max(time.Duration(rand.Float64()*float64(window)), retryAfter)
}

// pace waits d before the job's next request, cut short to leave one last
// look just before the caller gives up; false when no time is left.
func (c *client) pace(j *job, d time.Duration) bool {
	left := c.patience - time.Since(j.created) - 50*time.Millisecond
	if left <= 0 {
		j.attempts++
		return false
	}
	c.wait(j, min(d, left))
	return true
}

// submitJob creates the job on /jobs, returning its ID and when Aquifer
// says to check on it.
func (c *client) submitJob(j *job, timeout time.Duration) (string, int, time.Duration, error) {
	raw, _ := json.Marshal(map[string]any{
		"user_id":        j.tenant,
		"idempotent_key": j.id,
		"url":            c.backendURL,
		"method":         http.MethodPost,
		"headers":        map[string]string{"X-Tenant": j.tenant, "X-Job-Id": j.id},
		"max_retries":    -1,
	})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.target+"/jobs", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", j.tenant)
	c.request(j.tenant)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	var out struct {
		JobID string `json:"job_id"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return "", resp.StatusCode, serverWait(resp.Header), nil
	}
	return out.JobID, resp.StatusCode, serverWait(resp.Header), nil
}

// checkJob asks Aquifer where a job stands: state is "ok" or "failed" once
// it has finished, "waiting" before, with the Retry-After Aquifer gave.
func (c *client) checkJob(j *job, jobID string, timeout time.Duration) (string, int, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.target+"/jobs/"+jobID, nil)
	c.request(j.tenant)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	wait := serverWait(resp.Header)
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", resp.StatusCode, wait, nil
	}
	var out struct {
		Status string `json:"status"`
		Result *struct {
			ResponseStatus int `json:"response_status"`
		} `json:"result"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	switch out.Status {
	case "completed":
		if out.Result == nil || out.Result.ResponseStatus == 0 || out.Result.ResponseStatus == http.StatusOK {
			return "ok", resp.StatusCode, 0, nil
		}
		return "failed", resp.StatusCode, 0, nil
	case "failed":
		return "failed", resp.StatusCode, 0, nil
	}
	return "waiting", resp.StatusCode, wait, nil
}

// retry sends the job until it succeeds, backing off between attempts, or
// until MAX_ATTEMPTS, the job's enforced deadline or PATIENCE runs out.
func (c *client) retry(j *job, send func(time.Duration) (int, time.Duration, error)) result {
	for attempt := 1; ; attempt++ {
		timeout := c.timeout
		if c.enforce && !j.deadline.IsZero() {
			timeout = min(timeout, time.Until(j.deadline))
			if timeout <= 0 {
				return result{why: "deadline"}
			}
		}
		if c.patience > 0 {
			timeout = min(timeout, c.patience-time.Since(j.created))
			if timeout <= 0 {
				return result{why: "gave_up"}
			}
		}
		status, retryAfter, err := send(timeout)
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
		if c.maxAttempts > 0 && attempt >= c.maxAttempts {
			return result{why: why}
		}
		if !c.backoff(j, attempt, retryAfter) {
			if c.patience > 0 {
				return result{why: "gave_up"}
			}
			return result{why: "deadline"}
		}
	}
}

// proxy makes one /proxy attempt and reports it as a status code: 200 once
// the job completed with the backend's 200, whether Aquifer ran it straight
// away or held it and streamed the outcome.
func (c *client) proxy(j *job, timeout time.Duration) (int, time.Duration, error) {
	key := j.id
	if c.noKey {
		key = fmt.Sprintf("%s#%d", j.id, j.attempts)
	}
	raw, _ := json.Marshal(map[string]any{
		"user_id":        j.tenant,
		"idempotent_key": key,
		"url":            c.backendURL,
		"method":         http.MethodPost,
		"headers":        map[string]string{"X-Tenant": j.tenant, "X-Job-Id": j.id},
		"max_retries":    -1,
	})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.target+"/proxy", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", j.tenant)
	c.request(j.tenant)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, serverWait(resp.Header), nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The backend's own answer, or a repeat of a job already finished.
		var out struct {
			Status    string `json:"status"`
			Duplicate bool   `json:"duplicate"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
		if out.Duplicate && out.Status != "completed" {
			return 0, 0, fmt.Errorf("aquifer_%s", out.Status)
		}
		return http.StatusOK, 0, nil
	}
	// Held in Aquifer's queue: wait on its events for the outcome.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 4<<10), 1<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:") && event == "poll":
			// Too far back to be held: come back when Aquifer says.
			var out struct {
				RetryAfterMs int64 `json:"retry_after_ms"`
			}
			json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &out)
			return http.StatusAccepted, time.Duration(out.RetryAfterMs) * time.Millisecond, nil
		case strings.HasPrefix(line, "data:") && (event == "completed" || event == "failed"):
			var out struct {
				ResponseStatus int `json:"response_status"`
			}
			json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &out)
			if event == "completed" && (out.ResponseStatus == 0 || out.ResponseStatus == http.StatusOK) {
				return http.StatusOK, 0, nil
			}
			return 0, 0, fmt.Errorf("aquifer_%s", event)
		}
	}
	if ctx.Err() != nil {
		return 0, 0, ctx.Err()
	}
	return 0, 0, errors.New("stream_ended")
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
	if c.pool != "" {
		delete(body, "url")
		body["pool_id"] = c.pool
	}
	if c.enforce && !j.deadline.IsZero() {
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
		if c.maxAttempts > 0 && attempt >= c.maxAttempts {
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
	req.Header.Set("X-Tenant", body["user_id"].(string)) // for a gateway in front of Aquifer
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
// window, but never less than Retry-After; an aggressive tenant waits
// AGGRESSIVE_DELAY instead. It returns false when the job's enforced
// deadline (ENFORCE_DEADLINE) or the caller's PATIENCE would run out first.
func (c *client) backoff(j *job, attempt int, retryAfter time.Duration) bool {
	window := min(c.cap, time.Duration(float64(c.base)*math.Pow(2, float64(min(attempt-1, 30)))))
	d := max(time.Duration(rand.Float64()*float64(window)), retryAfter)
	switch c.styles[j.tenant] {
	case "aggressive":
		d = c.aggressive
	case "nojitter":
		d = max(window, retryAfter)
	}
	if c.enforce && !j.deadline.IsZero() && time.Now().Add(d).After(j.deadline) {
		j.attempts++
		return false
	}
	if c.patience > 0 && time.Since(j.created)+d >= c.patience {
		j.attempts++
		return false
	}
	c.wait(j, d)
	return true
}

// wait sleeps d before the job's next attempt and counts it.
func (c *client) wait(j *job, d time.Duration) {
	j.attempts++
	c.mu.Lock()
	t := c.tallies[j.tenant]
	t.MostAttempts = max(t.MostAttempts, j.attempts+1)
	t.BackoffSeconds += d.Seconds()
	c.mu.Unlock()
	time.Sleep(d)
}

func (c *client) request(tenant string) {
	c.mu.Lock()
	c.tallies[tenant].Requests++
	c.sent[tenant]++
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
	return &tally{Failed: map[string]int{}, latency: map[string][]float64{}, firstAnswer: map[string][]float64{}, chain: map[string][]float64{}, attempts: map[string][]float64{}}
}

func (t *tally) summarize() {
	t.LatencyP50, t.LatencyP99 = percentiles(t.latency)
	t.FirstAnswerP50, t.FirstAnswerP99 = percentiles(t.firstAnswer)
	t.AttemptsP50, t.AttemptsP99 = percentiles(t.attempts)
	if t.Chains > 0 {
		t.ChainP50, t.ChainP99 = percentiles(t.chain)
	}
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

// serverWait is when Aquifer said to come back: X-Aqueduct-Retry-After-Ms
// if it sent one, else Retry-After.
func serverWait(h http.Header) time.Duration {
	if ms, err := strconv.ParseInt(h.Get("X-Aqueduct-Retry-After-Ms"), 10, 64); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return retryAfter(h.Get("Retry-After"))
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
		f := strings.Split(strings.TrimSpace(part), ":")
		if len(f) < 2 || len(f) > 4 {
			log.Fatalf("TENANTS: bad entry %q", part)
		}
		r, err := strconv.ParseFloat(f[1], 64)
		style, workers := "polite", 10
		if len(f) >= 3 {
			style = f[2]
		}
		var err2 error
		if len(f) == 4 {
			workers, err2 = strconv.Atoi(f[3])
		}
		switch style {
		case "polite", "nojitter", "aggressive", "stack":
		default:
			err = fmt.Errorf("style %q", style)
		}
		if err != nil || err2 != nil || workers < 1 {
			log.Fatalf("TENANTS: bad entry %q", part)
		}
		out = append(out, tenant{f[0], r, style, workers})
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
