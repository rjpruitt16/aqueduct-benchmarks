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
//	ADVERTISE          1 to send X-Aqueduct-Rps / X-Aqueduct-Max-Concurrent
//	START_AT           Unix seconds the run starts (default now)
//	OUT                directory for results (default /out)
//	LISTEN             address (default :8080)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
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
	advertise bool

	seen    map[string]int
	tenants map[string]*tenantStats
	out     string
}

func main() {
	b := &backend{
		workers:   envInt("WORKERS", 8),
		queueMax:  envInt("QUEUE_MAX", 64),
		serviceMs: float64(envInt("SERVICE_MS", 200)),
		advertise: os.Getenv("ADVERTISE") == "1",
		seen:      map[string]int{},
		tenants:   map[string]*tenantStats{},
		out:       envStr("OUT", "/out"),
	}
	b.cond = sync.NewCond(&b.mu)
	if err := os.MkdirAll(filepath.Join(b.out, "clients"), 0o755); err != nil {
		log.Fatal(err)
	}
	start := time.Now()
	if s := os.Getenv("START_AT"); s != "" {
		sec, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			log.Fatalf("START_AT: %v", err)
		}
		start = time.Unix(sec, 0)
	}
	go b.schedule(start, os.Getenv("CAPACITY_SCHEDULE"))
	go b.sample(start)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /work", b.work)
	mux.HandleFunc("POST /report/{name}", b.report)
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(b.summary())
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	addr := envStr("LISTEN", ":8080")
	log.Printf("backend: %d workers, %.0f ms, queue %d, listening on %s", b.workers, b.serviceMs, b.queueMax, addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (b *backend) work(w http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get("X-Tenant")
	job := r.Header.Get("X-Job-Id")
	io.Copy(io.Discard, r.Body)

	b.mu.Lock()
	t := b.tenant(tenant)
	if b.waiting >= b.queueMax {
		t.Rejected++
		b.setHeaders(w)
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
	}
	if r.Context().Err() != nil {
		t.Orphaned++
	}
	b.setHeaders(w)
	b.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"job":%q,"ms":%d}`, job, d.Milliseconds())
}

// setHeaders advertises current capacity in Aqueduct's headers. Callers
// that don't speak Aqueduct (Envoy) ignore them. b.mu must be held.
func (b *backend) setHeaders(w http.ResponseWriter) {
	if !b.advertise {
		return
	}
	w.Header().Set("X-Aqueduct-Max-Concurrent", strconv.Itoa(b.workers))
	w.Header().Set("X-Aqueduct-Rps", strconv.FormatFloat(float64(b.workers)*1000/b.serviceMs, 'f', 1, 64))
}

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
		b.mu.Unlock()
		line["tenants"] = delta
		enc.Encode(line)
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
