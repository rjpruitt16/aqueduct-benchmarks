// Command driver offers documents to Hatchet on a phased schedule for several
// tenants, records each one in a ledger from the moment it is offered, polls
// run status the way applications do, and writes per-interval metrics and a
// final report.
//
// Environment:
//
//	HATCHET_API           Hatchet REST base URL (default http://localhost:8888)
//	HATCHET_CLIENT_TOKEN  Hatchet API token (the tenant is read from its sub claim)
//	RATE                  documents/s at 100% of calibrated capacity (default 50)
//	PHASES                name:seconds:multiplier,... (default the agreed 75-minute run)
//	SCALE                 multiply every phase length (default 1; e.g. 0.05 locally)
//	LISTEN                callback listen address (default :8080)
//	OUT                   output directory (default ./out)
//	SUBMITTERS            concurrent trigger requests per tenant (default 32)
//	POLL_EVERY            per-document status poll interval (default 5s)
//	POLLERS               concurrent status polls (default 32)
//	MAX_ATTEMPTS          trigger attempts before a document is rejected (default 6)
//	DRAIN_MAX             how long to wait for outstanding work after the last phase (default 30m)
//	AQUIFER_URL           submit through Aquifer at this base URL (C0, C); unset sends straight to Hatchet
//	PUBLIC_URL            this driver's address as Aquifer reaches it (default http://localhost:8080)
//	AQUIFER_MAX_RETRIES   Aquifer's retries forwarding each job to Hatchet (default 20)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rjpruitt16/aqueduct-benchmarks/hatchet/internal/pipeline"
)

const defaultPhases = "steady:600:0.5,burst:600:3,sustained:1200:1.5,recovery:1800:0.5"

type phase struct {
	name       string
	length     time.Duration
	multiplier float64
}

// tenant is one producer. Everyone except the law firm offers a fixed share
// of capacity in every phase; the law firm offers the rest, so bursts are its
// batches landing on top of everyone's ordinary traffic.
type tenant struct {
	name   string
	fixed  float64 // fraction of RATE offered in every phase; 0 for the law firm
	isBulk bool
	pages  int
}

var tenants = []tenant{
	{name: "law-firm", isBulk: true, pages: 40},
	{name: "accounting", fixed: 0.15, pages: 10},
	{name: "small-1", fixed: 0.02, pages: 3},
	{name: "small-2", fixed: 0.02, pages: 3},
	{name: "small-3", fixed: 0.02, pages: 3},
	{name: "small-4", fixed: 0.02, pages: 3},
	{name: "small-5", fixed: 0.02, pages: 3},
}

func main() {
	api := env("HATCHET_API", "http://localhost:8888")
	token := os.Getenv("HATCHET_CLIENT_TOKEN")
	tenantID, err := tenantFromToken(token)
	if err != nil {
		log.Fatalf("HATCHET_CLIENT_TOKEN: %v", err)
	}
	rate := envFloat("RATE", 50)
	phases, err := parsePhases(env("PHASES", defaultPhases), envFloat("SCALE", 1))
	if err != nil {
		log.Fatalf("PHASES: %v", err)
	}
	out := env("OUT", "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		log.Fatal(err)
	}

	h := &hatchetAPI{base: strings.TrimRight(api, "/"), token: token, tenant: tenantID,
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 512}}}
	l := newLedger()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Completions from the index step.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /done", func(w http.ResponseWriter, r *http.Request) {
		var c pipeline.Completion
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		l.finish(c.DocID, outcomeCompleted, time.Now())
		w.WriteHeader(http.StatusNoContent)
	})
	// Aquifer's webhook: its forward to Hatchet finished.
	mux.HandleFunc("POST /aquifer", func(w http.ResponseWriter, r *http.Request) {
		var hook aquiferWebhook
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&hook); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, known := l.docForJob(hook.JobID)
		if !known {
			// Aquifer dispatched before submit recorded the job ID. Answer
			// 503 so Aquifer redelivers once the ledger knows the job.
			http.Error(w, "job not recorded yet", http.StatusServiceUnavailable)
			return
		}
		if id != "" {
			if runID := hook.runID(); runID != "" {
				l.attachRun(hook.JobID, runID, time.Now())
			} else {
				l.finish(id, outcomeGaveUp, time.Now())
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Addr: env("LISTEN", ":8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("callback server: %v", err)
		}
	}()
	defer srv.Close()

	// Each tenant is its own producer with its own submitters, as separate
	// applications would be: one tenant's slow or refused submissions never
	// hold up another tenant's documents in the driver.
	r := &run{h: h, l: l, rate: rate, maxAttempts: envInt("MAX_ATTEMPTS", 6),
		queues: make(map[string]chan *doc, len(tenants))}
	for _, t := range tenants {
		r.queues[t.name] = make(chan *doc, 1<<20)
	}
	if base := os.Getenv("AQUIFER_URL"); base != "" {
		r.aq = &aquiferAPI{base: strings.TrimRight(base, "/"), hatchet: h,
			webhookURL: strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/") + "/aquifer",
			maxRetries: envInt("AQUIFER_MAX_RETRIES", 20), http: h.http}
		log.Printf("submitting through Aquifer at %s", base)
	}
	var wg sync.WaitGroup
	for _, q := range r.queues {
		for i := 0; i < envInt("SUBMITTERS", 32); i++ {
			wg.Add(1)
			go func() { defer wg.Done(); r.submitLoop(ctx, q) }()
		}
	}
	pollCtx, stopPolling := context.WithCancel(ctx)
	go r.pollLoop(pollCtx, envDuration("POLL_EVERY", 5*time.Second), envInt("POLLERS", 32))
	reportCtx, stopReporting := context.WithCancel(context.Background())
	reportDone := make(chan struct{})
	go func() { r.reportLoop(reportCtx, filepath.Join(out, "intervals.jsonl")); close(reportDone) }()

	start := time.Now()
	r.offerPhases(ctx, phases)
	overloadEnd := time.Now()
	for _, q := range r.queues {
		close(q)
	}
	wg.Wait()

	// Let accepted work finish, bounded by DRAIN_MAX.
	drainDeadline := time.Now().Add(envDuration("DRAIN_MAX", 30*time.Minute))
	for l.outstanding() > 0 && time.Now().Before(drainDeadline) && ctx.Err() == nil {
		time.Sleep(time.Second)
	}
	stopPolling()
	stopReporting()
	<-reportDone

	rep := buildReport(l, phases, start, overloadEnd)
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "summary.json"), b, 0o644); err != nil {
		log.Printf("write summary: %v", err)
	}
	fmt.Println(string(b))
}

type run struct {
	h           *hatchetAPI
	aq          *aquiferAPI // nil: submit straight to Hatchet
	l           *ledger
	rate        float64
	maxAttempts int
	queues      map[string]chan *doc // per tenant
	seq         atomic.Int64
	phase       atomic.Value // current phase name
	retries     atomic.Int64
}

// offerPhases generates documents for every tenant at its rate for each phase.
// Documents wait in their tenant's queue when its submitters are busy; that wait
// counts toward their latency, since they were offered when generated.
func (r *run) offerPhases(ctx context.Context, phases []phase) {
	const tick = 100 * time.Millisecond
	owed := make([]float64, len(tenants))
	for _, p := range phases {
		r.phase.Store(p.name)
		log.Printf("phase %s: %.0f%% of %.1f docs/s for %s", p.name, p.multiplier*100, r.rate, p.length)
		fixedTotal := 0.0
		for _, t := range tenants {
			fixedTotal += t.fixed
		}
		end := time.Now().Add(p.length)
		ticker := time.NewTicker(tick)
		for time.Now().Before(end) {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
			}
			for i, t := range tenants {
				share := t.fixed
				if t.isBulk {
					share = p.multiplier - fixedTotal
					if share < 0 {
						share = 0
					}
				}
				owed[i] += share * r.rate * tick.Seconds()
				for owed[i] >= 1 {
					owed[i]--
					d := &doc{id: fmt.Sprintf("d%d", r.seq.Add(1)), tenant: t.name, offered: time.Now()}
					r.l.offer(d)
					r.queues[t.name] <- d
				}
			}
		}
		ticker.Stop()
	}
}

// submitLoop triggers queued documents, retrying with exponential backoff and
// honoring Retry-After, the way a careful producer would.
func (r *run) submitLoop(ctx context.Context, queue <-chan *doc) {
	for d := range queue {
		pages := 3
		for _, t := range tenants {
			if t.name == d.tenant {
				pages = t.pages
			}
		}
		in := pipeline.Document{DocID: d.id, Tenant: d.tenant, Pages: pages}
		backoff := time.Second
		for attempt := 1; ; attempt++ {
			if ctx.Err() != nil {
				return
			}
			var err error
			if r.aq != nil {
				var jobID string
				if jobID, err = r.aq.submit(ctx, in); err == nil {
					r.l.acceptByAquifer(d.id, jobID, attempt, time.Now())
					break
				}
			} else {
				var runID string
				if runID, err = r.h.trigger(ctx, in); err == nil {
					r.l.accept(d.id, runID, attempt, time.Now())
					break
				}
			}
			retryable, ok := err.(*errRetryable)
			if !ok || attempt >= r.maxAttempts {
				log.Printf("rejected %s after %d attempt(s): %v", d.id, attempt, err)
				r.l.finish(d.id, outcomeRejected, time.Now())
				break
			}
			r.retries.Add(1)
			wait := backoff + time.Duration(rand.Int64N(int64(backoff)))
			if retryable.retryAfter > wait {
				wait = retryable.retryAfter
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
}

// pollLoop checks the status of accepted, unfinished documents, which is
// both how failures and never-started timeouts are found and a realistic
// source of read load on Hatchet.
func (r *run) pollLoop(ctx context.Context, every time.Duration, workers int) {
	sem := make(chan struct{}, workers)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, d := range r.l.due(every, time.Now(), 4*workers) {
			sem <- struct{}{}
			go func(d *doc) {
				defer func() { <-sem }()
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				s, err := r.h.status(pctx, d.runID)
				if err != nil {
					return
				}
				switch s {
				case "COMPLETED":
					// The callback should have arrived; record it from the
					// poll if it didn't.
					r.l.finish(d.id, outcomeCompleted, time.Now())
				case "FAILED", "CANCELLED":
					never, err := r.h.neverStarted(pctx, d.runID)
					if err != nil {
						return
					}
					if never {
						r.l.finish(d.id, outcomeNeverStarted, time.Now())
					} else {
						r.l.finish(d.id, outcomeFailed, time.Now())
					}
				}
			}(d)
		}
	}
}

func parsePhases(spec string, scale float64) ([]phase, error) {
	var out []phase
	for _, part := range strings.Split(spec, ",") {
		f := strings.Split(strings.TrimSpace(part), ":")
		if len(f) != 3 {
			return nil, fmt.Errorf("phase %q: want name:seconds:multiplier", part)
		}
		secs, err1 := strconv.ParseFloat(f[1], 64)
		mult, err2 := strconv.ParseFloat(f[2], 64)
		if err1 != nil || err2 != nil || secs <= 0 || mult < 0 {
			return nil, fmt.Errorf("phase %q: bad number", part)
		}
		out = append(out, phase{name: f[0], length: time.Duration(secs * scale * float64(time.Second)), multiplier: mult})
	}
	return out, nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return fallback
}

func envFloat(name string, fallback float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v > 0 {
		return v
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return fallback
}
