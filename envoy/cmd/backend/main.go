// Command backend is a synthetic service with a fixed number of workers,
// standing in for any API whose capacity is limited: a database-backed
// service, an inference server, a third-party API.
//
// Each POST /work holds one worker for SERVICE_MS (±20%). Requests that find
// every worker busy wait in line; past QUEUE_MAX waiting, the backend answers
// 503 with Retry-After: 1 straight away. When the line is longer than the
// number of workers, work slows down (up to 3x), as a real service does when
// it thrashes. Work that has started always finishes, even when the caller
// has given up: that is how a retry turns into duplicated work.
//
// Every execution is counted by its X-Job-Id, so duplicates are exact.
//
// Configuration (environment):
//
//	WORKERS            concurrent capacity at the start (default 8)
//	SERVICE_MS         base service time (default 200)
//	QUEUE_MAX          waiting requests before 503 (default 64)
//	CAPACITY_SCHEDULE  capacity changes after START_AT, e.g. "20m:16"
//	ADVERTISE          how to speak Aqueduct's headers (Envoy ignores them):
//	                   "" none; "1" the fixed capacity on every response;
//	                   "live" the current spare capacity, split across the
//	                   caller's X-Aqueduct-Active-Queues (see liveHeaders)
//	BACKLOG_SECONDS    with ADVERTISE=live, X-Aqueduct-Max-Backlog is this
//	                   many seconds of capacity (default 60)
//	REGISTER_URL       an Aquifer pool's members URL (or REGISTER_URL in the
//	                   run configuration); if set, the backend registers
//	                   itself there every 2 s with its current capacity,
//	                   instead of sending pacing headers
//	ONLINE_AFTER       stay off the network until this long after START_AT,
//	                   e.g. "15m": a machine joining the fleet mid-run, at
//	                   the same moment for every front door
//	START_AT           Unix seconds the run starts; otherwise the START_AT in
//	                   the run configuration POSTed to /config, or, with
//	                   CONFIG_URL, fetched from another backend
//	OUT                directory for results (default /out)
//	LISTEN             address (default :8080)
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type tenantStats struct {
	Executions int `json:"executions"`
	Duplicates int `json:"duplicates"`
	Orphaned   int `json:"orphaned"`
	Rejected   int `json:"rejected"`
}

type backend struct {
	mu        sync.Mutex
	cond      *sync.Cond
	workers   int
	busy      int
	waiting   int
	queueMax  int
	serviceMs float64
	advertise string
	backlogS  float64
	pooled    bool

	seen    map[string]int
	tenants map[string]*tenantStats
	out     string

	config    []byte
	configSet chan struct{}
	reportMu  sync.Mutex

	// Every execution's job ID, and machine events (start, resume), so a run
	// with several backend machines can count duplicates across them and
	// time when each one came online.
	execs  *bufio.Writer
	events *json.Encoder
}

func main() {
	b := &backend{
		workers:   envInt("WORKERS", 8),
		queueMax:  envInt("QUEUE_MAX", 64),
		serviceMs: float64(envInt("SERVICE_MS", 200)),
		advertise: os.Getenv("ADVERTISE"),
		backlogS:  float64(envInt("BACKLOG_SECONDS", 60)),
		seen:      map[string]int{},
		tenants:   map[string]*tenantStats{},
		out:       envStr("OUT", "/out"),
		configSet: make(chan struct{}),
	}
	b.cond = sync.NewCond(&b.mu)
	if err := os.MkdirAll(filepath.Join(b.out, "clients"), 0o755); err != nil {
		log.Fatal(err)
	}
	execs, err := os.Create(filepath.Join(b.out, "executions.txt"))
	if err != nil {
		log.Fatal(err)
	}
	b.execs = bufio.NewWriter(execs)
	events, err := os.Create(filepath.Join(b.out, "events.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	b.events = json.NewEncoder(events)
	b.event("start", 0)
	go b.watchResume()
	if u := os.Getenv("CONFIG_URL"); u != "" && os.Getenv("START_AT") == "" {
		go b.fetchConfig(u)
	}
	online := make(chan struct{})
	go func() {
		s := os.Getenv("START_AT")
		var cfg map[string]string
		if s == "" {
			<-b.configSet
			json.Unmarshal(b.config, &cfg)
			s = cfg["START_AT"]
		}
		sec, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			log.Fatalf("START_AT: %v", err)
		}
		start := time.Unix(sec, 0)
		go b.sample(start)
		go b.schedule(start, os.Getenv("CAPACITY_SCHEDULE"))
		<-online
		if u := envStr("REGISTER_URL", cfg["REGISTER_URL"]); u != "" {
			b.mu.Lock()
			b.pooled = true
			b.mu.Unlock()
			go b.register(u)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /work", b.work)
	mux.HandleFunc("POST /report/{name}", b.report)
	// The run configuration the clients share, set once every client is up.
	// It is set once (publishing the same settings again is fine), and its
	// START_AT must still be ahead, so every client and the capacity
	// schedule share the same clock.
	mux.HandleFunc("POST /config", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		same := b.config != nil && bytes.Equal(b.config, raw)
		b.mu.Unlock()
		if same { // a retried publish
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var cfg map[string]string
		if err := json.Unmarshal(raw, &cfg); err != nil {
			http.Error(w, "config: "+err.Error(), http.StatusBadRequest)
			return
		}
		if os.Getenv("START_AT") == "" {
			sec, err := strconv.ParseInt(cfg["START_AT"], 10, 64)
			if err != nil || !time.Unix(sec, 0).After(time.Now()) {
				http.Error(w, "config: START_AT must be a Unix time still ahead", http.StatusBadRequest)
				return
			}
		}
		b.mu.Lock()
		if b.config != nil {
			b.mu.Unlock()
			http.Error(w, "config already set", http.StatusConflict)
			return
		}
		b.config = raw
		b.mu.Unlock()
		close(b.configSet)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		raw := b.config
		b.mu.Unlock()
		if raw == nil {
			http.Error(w, "not set yet", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(b.summary())
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	addr := envStr("LISTEN", ":8080")
	if after := os.Getenv("ONLINE_AFTER"); after != "" {
		d, err := time.ParseDuration(after)
		if err != nil {
			log.Fatalf("ONLINE_AFTER: %v", err)
		}
		<-b.configSet
		var cfg map[string]string
		json.Unmarshal(b.config, &cfg)
		sec, _ := strconv.ParseInt(cfg["START_AT"], 10, 64)
		time.Sleep(time.Until(time.Unix(sec, 0).Add(d)))
		b.event("online", 0)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	close(online)
	log.Printf("backend: %d workers, %.0f ms, queue %d, listening on %s", b.workers, b.serviceMs, b.queueMax, addr)
	log.Fatal(http.Serve(ln, mux))
}

func (b *backend) work(w http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get("X-Tenant")
	job := r.Header.Get("X-Job-Id")
	io.Copy(io.Discard, r.Body)

	b.mu.Lock()
	t := b.tenant(tenant)
	if b.waiting >= b.queueMax {
		t.Rejected++
		b.setHeaders(w, r)
		b.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	b.waiting++
	for b.busy >= b.workers {
		b.cond.Wait()
	}
	b.waiting--
	line := b.waiting
	b.busy++
	workers := b.workers
	b.mu.Unlock()

	// Thrashing: past one full line of waiting work, each job takes longer.
	slow := 1 + 0.5*max(0, float64(line)/float64(workers)-1)
	d := time.Duration(b.serviceMs * (0.8 + 0.4*rand.Float64()) * min(slow, 3) * float64(time.Millisecond))
	time.Sleep(d)

	b.mu.Lock()
	b.busy--
	b.cond.Signal()
	t.Executions++
	if job != "" {
		b.seen[job]++
		if b.seen[job] > 1 {
			t.Duplicates++
		}
		b.execs.WriteString(job + "\n")
	}
	if r.Context().Err() != nil {
		t.Orphaned++
	}
	b.setHeaders(w, r)
	b.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"job":%q,"ms":%d}`, job, d.Milliseconds())
}

// setHeaders advertises capacity in Aqueduct's headers. Callers that don't
// speak Aqueduct (Envoy) ignore them. A pooled backend reports its capacity
// by registering instead: a pool shares one queue's pace across members, so
// one member's header would set the pace for all of them. b.mu must be held.
func (b *backend) setHeaders(w http.ResponseWriter, r *http.Request) {
	switch {
	case b.pooled:
	case b.advertise == "1":
		w.Header().Set("X-Aqueduct-Max-Concurrent", strconv.Itoa(b.workers))
		w.Header().Set("X-Aqueduct-Rps", strconv.FormatFloat(float64(b.workers)*1000/b.serviceMs, 'f', 1, 64))
	case b.advertise == "live":
		b.liveHeaders(w.Header(), r.Header.Get("X-Aqueduct-Active-Queues"))
	}
}

// liveHeaders is what a service that knows its own load tells each caller.
//
// Capacity is workers/service time, scaled down once requests wait: by
// workers/(busy+waiting), so a line as long as the workers halves it. The
// caller sends X-Aqueduct-Active-Queues, the number of its queues sharing
// this service; each queue gets that share of the rate and of the workers,
// so all of them together send what the service can do, not N times it.
// X-Aqueduct-Max-Backlog bounds the work the caller holds for this service
// to BACKLOG_SECONDS of capacity. b.mu must be held.
func (b *backend) liveHeaders(h http.Header, activeQueues string) {
	n, err := strconv.Atoi(activeQueues)
	if err != nil || n < 1 {
		n = 1
	}
	rate := b.capacity()
	h.Set("X-Aqueduct-Rps", strconv.FormatFloat(rate/float64(n), 'f', 2, 64))
	h.Set("X-Aqueduct-Max-Concurrent", strconv.Itoa(max(1, (b.workers+n-1)/n)))
	h.Set("X-Aqueduct-Max-Backlog", strconv.Itoa(int(float64(b.workers)*1000/b.serviceMs*b.backlogS)))
}

// capacity is the rate the backend can take now, in jobs/s. b.mu must be held.
func (b *backend) capacity() float64 {
	rate := float64(b.workers) * 1000 / b.serviceMs
	if load := b.busy + b.waiting; load > b.workers {
		rate *= float64(b.workers) / float64(load)
	}
	return rate
}

// register keeps this backend in an Aquifer pool, every 2 s, with its
// current capacity. Aquifer drops a member that misses three heartbeats, so
// a suspended machine leaves the pool and rejoins when it resumes.
func (b *backend) register(url string) {
	self := fmt.Sprintf("http://%s.vm.%s.internal:8080/work", os.Getenv("FLY_MACHINE_ID"), os.Getenv("FLY_APP_NAME"))
	id := envStr("FLY_MACHINE_ID", "backend")
	for range time.Tick(2 * time.Second) {
		b.mu.Lock()
		rate := b.capacity()
		b.mu.Unlock()
		body, _ := json.Marshal(map[string]any{"member_id": id, "address": self, "capacity_rps": rate, "heartbeat_interval_seconds": 2})
		if resp, err := control.Post(url, "application/json", bytes.NewReader(body)); err == nil {
			resp.Body.Close()
		}
	}
}

// watchResume records when the machine comes back from being suspended:
// a gap in a 100 ms ticker much longer than the tick.
func (b *backend) watchResume() {
	last := time.Now()
	for now := range time.Tick(100 * time.Millisecond) {
		if gap := now.Sub(last); gap > 2*time.Second {
			b.event("resumed", gap.Seconds())
		}
		last = now
	}
}

func (b *backend) event(name string, gapS float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "event": name, "workers": b.workers}
	if gapS > 0 {
		e["gap_s"] = gapS
	}
	b.events.Encode(e)
	log.Printf("event: %s", name)
}

// fetchConfig polls another backend for the run's settings, for a backend
// that isn't the one the run publishes them to.
func (b *backend) fetchConfig(url string) {
	for {
		if resp, err := control.Get(url); err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				b.mu.Lock()
				first := b.config == nil
				if first {
					b.config = raw
				}
				b.mu.Unlock()
				if first {
					close(b.configSet)
				}
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// control carries the run's own traffic, bounded so a stuck peer can't hang it.
var control = &http.Client{Timeout: 5 * time.Second}

func (b *backend) tenant(name string) *tenantStats {
	t := b.tenants[name]
	if t == nil {
		t = &tenantStats{}
		b.tenants[name] = t
	}
	return t
}

// schedule applies capacity changes such as "20m:16,40m:8" relative to start.
func (b *backend) schedule(start time.Time, spec string) {
	for _, step := range strings.Split(spec, ",") {
		at, n, ok := strings.Cut(strings.TrimSpace(step), ":")
		if !ok {
			continue
		}
		after, err1 := time.ParseDuration(at)
		workers, err2 := strconv.Atoi(n)
		if err1 != nil || err2 != nil || workers < 1 {
			log.Printf("ignoring capacity step %q", step)
			continue
		}
		time.Sleep(time.Until(start.Add(after)))
		b.mu.Lock()
		b.workers = workers
		b.cond.Broadcast()
		b.mu.Unlock()
		log.Printf("capacity: %d workers", workers)
	}
}

// sample writes one line every 10 s: capacity, line length, and work done.
func (b *backend) sample(start time.Time) {
	f, err := os.Create(filepath.Join(b.out, "backend.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	enc := json.NewEncoder(f)
	prev := map[string]tenantStats{}
	for range time.Tick(10 * time.Second) {
		b.mu.Lock()
		line := map[string]any{
			"at": time.Now().UTC().Format(time.RFC3339), "t": int(time.Since(start).Seconds()),
			"workers": b.workers, "busy": b.busy, "waiting": b.waiting,
		}
		delta := map[string]tenantStats{}
		for name, t := range b.tenants {
			p := prev[name]
			delta[name] = tenantStats{t.Executions - p.Executions, t.Duplicates - p.Duplicates, t.Orphaned - p.Orphaned, t.Rejected - p.Rejected}
			prev[name] = *t
		}
		b.execs.Flush()
		b.mu.Unlock()
		line["tenants"] = delta
		enc.Encode(line)
		summary, _ := json.Marshal(b.summary())
		b.reportMu.Lock()
		os.WriteFile(filepath.Join(b.out, "backend-summary.json"), summary, 0o644)
		b.reportMu.Unlock()
	}
}

func (b *backend) summary() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	tenants := map[string]tenantStats{}
	for name, t := range b.tenants {
		tenants[name] = *t
	}
	return map[string]any{"workers": b.workers, "unique_jobs": len(b.seen), "tenants": tenants}
}

// report stores a client's final report; the run collects them from OUT.
func (b *backend) report(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(filepath.Join(b.out, "clients", name+".json"), raw, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	summary, _ := json.Marshal(b.summary())
	b.reportMu.Lock()
	defer b.reportMu.Unlock()
	os.WriteFile(filepath.Join(b.out, "backend-summary.json"), summary, 0o644)
	w.WriteHeader(http.StatusNoContent)
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
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
