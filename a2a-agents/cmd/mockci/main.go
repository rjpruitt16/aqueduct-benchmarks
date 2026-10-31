// Command mockci stands in for a hosted CI service (GitHub Actions,
// Buildkite): starting a run answers at once with its ID; the run waits for a
// runner, takes a while, and the caller polls for its outcome.
//
//	POST /runs              {key, workflow, tenant, attempt}: start a run. The same key
//	                        returns the same run (200), a new key a new run (201)
//	GET  /runs/{id}         the run's status: queued, running, passed or failed
//	GET  /records/{id}      the same, unthrottled, for the driver's own check
//	POST /admin             {"paused": bool, "extra_ms": N}: runners stop taking runs,
//	                        or every run takes N ms longer
//	GET  /metrics           runners, queue, runs, duplicates, polls, refusals, flaky runs
//
// Starts and polls both count against the tenant's API budget: a token bucket
// of RATE_PER_MIN, holding up to BURST. Past it, 429 with Retry-After, like
// GitHub's secondary rate limits.
//
// How long a run takes and whether it fails by chance (FLAKY_PCT) are fixed
// by the workflow's sequence number and the attempt, so the same workflows
// take as long and flake the same way in every configuration. A flaky
// failure passes when run again under a new key.
//
// Environment: LISTEN (:8500), RUNNERS (4), DURATION ("30s-90s"), FLAKY_PCT (5),
// RATE_PER_MIN (60), BURST (10).
package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type run struct {
	ID       string    `json:"run_id"`
	Key      string    `json:"key"`
	Workflow string    `json:"workflow"`
	Tenant   string    `json:"tenant"`
	Attempt  int       `json:"attempt"`
	Status   string    `json:"status"` // queued, running, passed, failed
	Flaky    bool      `json:"flaky,omitempty"`
	Created  time.Time `json:"created"`
	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`

	duration time.Duration
}

type bucket struct {
	tokens float64
	last   time.Time
}

type server struct {
	minDur, maxDur time.Duration
	flakyPct       int
	perSec, burst  float64
	extraMs        atomic.Int64

	mu      sync.Mutex
	cond    *sync.Cond
	paused  bool
	runs    map[string]*run
	byKey   map[string]string
	queue   []*run
	running int
	buckets map[string]*bucket
	seq     int

	duplicates, polls, refused, flaky, finished atomic.Int64
	queueWaitMs                                 atomic.Int64
}

func main() {
	lo, hi, ok := strings.Cut(env("DURATION", "30s-90s"), "-")
	minDur, err1 := time.ParseDuration(lo)
	maxDur, err2 := time.ParseDuration(hi)
	if !ok || err1 != nil || err2 != nil || maxDur < minDur {
		log.Fatalf("DURATION must be min-max, like 30s-90s")
	}
	s := &server{minDur: minDur, maxDur: maxDur, flakyPct: envInt("FLAKY_PCT", 5),
		perSec: float64(envInt("RATE_PER_MIN", 60)) / 60, burst: float64(envInt("BURST", 10)),
		runs: map[string]*run{}, byKey: map[string]string{}, buckets: map[string]*bucket{}}
	s.cond = sync.NewCond(&s.mu)
	runners := envInt("RUNNERS", 4)
	for i := 0; i < runners; i++ {
		go s.runner()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runs", s.start)
	mux.HandleFunc("GET /runs/{id}", s.poll)
	mux.HandleFunc("GET /records/{id}", s.record)
	mux.HandleFunc("POST /admin", s.admin)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		queued, running, total := len(s.queue), s.running, len(s.runs)
		s.mu.Unlock()
		fmt.Fprintf(w, "ci_runners %d\nci_queued %d\nci_running %d\nci_runs_total %d\nci_runs_finished_total %d\n", runners, queued, running, total, s.finished.Load())
		fmt.Fprintf(w, "ci_duplicate_starts_total %d\nci_polls_total %d\nci_refused_total %d\nci_flaky_total %d\n", s.duplicates.Load(), s.polls.Load(), s.refused.Load(), s.flaky.Load())
		fmt.Fprintf(w, "ci_queue_wait_seconds_sum %.3f\n", float64(s.queueWaitMs.Load())/1000)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	addr := env("LISTEN", ":8500")
	log.Printf("mockci on %s: %d runners, runs %s-%s, %d%% flaky, %.0f calls/min per tenant (burst %.0f)",
		addr, runners, minDur, maxDur, s.flakyPct, s.perSec*60, s.burst)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// allow takes a token from the tenant's bucket, or says how long until one is there.
func (s *server) allow(tenant string) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	b := s.buckets[tenant]
	if b == nil {
		b = &bucket{tokens: s.burst, last: now}
		s.buckets[tenant] = b
	}
	b.tokens = math.Min(s.burst, b.tokens+now.Sub(b.last).Seconds()*s.perSec)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / s.perSec * float64(time.Second))
}

func (s *server) refuse(w http.ResponseWriter, wait time.Duration) {
	s.refused.Add(1)
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	http.Error(w, "API rate limit exceeded for this tenant", http.StatusTooManyRequests)
}

func (s *server) start(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key      string `json:"key"`
		Workflow string `json:"workflow"`
		Tenant   string `json:"tenant"`
		Attempt  int    `json:"attempt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || in.Key == "" || in.Tenant == "" {
		http.Error(w, "need key, workflow and tenant", http.StatusBadRequest)
		return
	}
	if ok, wait := s.allow(in.Tenant); !ok {
		s.refuse(w, wait)
		return
	}
	s.mu.Lock()
	if id, ok := s.byKey[in.Key]; ok {
		cp := *s.runs[id]
		s.mu.Unlock()
		s.duplicates.Add(1)
		writeJSON(w, http.StatusOK, cp)
		return
	}
	s.seq++
	ru := &run{ID: fmt.Sprintf("ci%d-%d", time.Now().Unix(), s.seq), Key: in.Key, Workflow: in.Workflow, Tenant: in.Tenant,
		Attempt: in.Attempt, Status: "queued", Created: time.Now().UTC()}
	ru.duration, ru.Flaky = s.fate(in.Workflow, in.Attempt)
	s.runs[ru.ID] = ru
	s.byKey[in.Key] = ru.ID
	s.queue = append(s.queue, ru)
	cp := *ru
	s.cond.Signal()
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, cp)
}

// fate fixes a run's length and whether it flakes from the workflow's
// sequence number (the part after the last dash) and the attempt.
func (s *server) fate(workflow string, attempt int) (time.Duration, bool) {
	seq := workflow
	if i := strings.LastIndex(workflow, "-"); i >= 0 {
		seq = workflow[i+1:]
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%s/%d", seq, attempt)
	v := h.Sum64()
	span := s.maxDur - s.minDur
	d := s.minDur
	if span > 0 {
		d += time.Duration(v % uint64(span/time.Millisecond+1) * uint64(time.Millisecond))
	}
	return d, int((v>>32)%100) < s.flakyPct
}

func (s *server) runner() {
	for {
		s.mu.Lock()
		for s.paused || len(s.queue) == 0 {
			s.cond.Wait()
		}
		ru := s.queue[0]
		s.queue = s.queue[1:]
		ru.Status, ru.Started = "running", time.Now().UTC()
		s.running++
		s.mu.Unlock()
		s.queueWaitMs.Add(ru.Started.Sub(ru.Created).Milliseconds())

		time.Sleep(ru.duration + time.Duration(s.extraMs.Load())*time.Millisecond)

		s.mu.Lock()
		ru.Status, ru.Finished = "passed", time.Now().UTC()
		if ru.Flaky {
			ru.Status = "failed"
			s.flaky.Add(1)
		}
		s.running--
		s.mu.Unlock()
		s.finished.Add(1)
	}
}

func (s *server) poll(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		http.Error(w, "need ?tenant=", http.StatusBadRequest)
		return
	}
	if ok, wait := s.allow(tenant); !ok {
		s.refuse(w, wait)
		return
	}
	s.polls.Add(1)
	s.record(w, r)
}

func (s *server) record(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ru, ok := s.runs[r.PathValue("id")]
	var cp run
	if ok {
		cp = *ru
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "no such run", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

func (s *server) admin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Paused  *bool  `json:"paused"`
		ExtraMs *int64 `json:"extra_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if in.ExtraMs != nil {
		s.extraMs.Store(*in.ExtraMs)
	}
	if in.Paused != nil {
		s.mu.Lock()
		s.paused = *in.Paused
		s.cond.Broadcast()
		s.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
