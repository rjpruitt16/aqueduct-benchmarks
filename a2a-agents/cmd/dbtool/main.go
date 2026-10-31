// Command dbtool is the database the agents use, as an HTTP tool service in
// front of Postgres with a bounded connection pool.
//
//	GET  /issues/{id}  the issue's module, tests and earlier attempts (reads)
//	POST /attempts     record a proposed fix {key, workflow, issue, attempt, fix}:
//	                   a side effect, stored once per key (a repeat is counted, not stored)
//	POST /admin        {"delay_ms": N} slows every query by N ms (holding its
//	                   connection, as a slow query does); {"paused": true} answers
//	                   503 to everything, as an unavailable database would
//	GET  /metrics      pool use, requests by route and status, duplicate writes
//
// A request waits up to DB_WAIT for a pool connection, then gets 503 with
// Retry-After: the pool, not the caller, sets the limit.
//
// Environment: LISTEN (:8100), DATABASE_URL, DB_POOL (8), DB_WAIT (2s).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/advertise"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/corpus"
)

type server struct {
	pool      *pgxpool.Pool
	wait      time.Duration
	delayMs   atomic.Int64
	paused    atomic.Bool
	mu        sync.Mutex
	requests  map[string]int64 // "route status" -> count
	stored    atomic.Int64
	duplicate atomic.Int64
	waiting   atomic.Int64
}

func main() {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("DATABASE_URL: %v", err)
	}
	cfg.MaxConns = int32(envInt("DB_POOL", 8))
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := seed(ctx, pool); err != nil {
		log.Fatalf("seed: %v", err)
	}
	wait, err := time.ParseDuration(envStr("DB_WAIT", "2s"))
	if err != nil {
		log.Fatalf("DB_WAIT: %v", err)
	}
	s := &server{pool: pool, wait: wait, requests: map[string]int64{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /issues/{id}", s.getIssue)
	mux.HandleFunc("POST /attempts", s.postAttempt)
	mux.HandleFunc("POST /admin", s.admin)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	addr := envStr("LISTEN", ":8100")
	log.Printf("dbtool on %s: pool %d, wait %s", addr, cfg.MaxConns, wait)
	log.Fatal(http.ListenAndServe(addr, advertise.Wrap(mux, int(cfg.MaxConns))))
}

func seed(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS issues (id text PRIMARY KEY, title text, function text, module text, tests text);
		CREATE TABLE IF NOT EXISTS attempts (key text PRIMARY KEY, workflow text, issue text, attempt int, fix text, at timestamptz DEFAULT now());
		CREATE INDEX IF NOT EXISTS attempts_issue ON attempts (issue)`); err != nil {
		return err
	}
	for _, i := range corpus.All() {
		if _, err := pool.Exec(ctx, `INSERT INTO issues VALUES ($1,$2,$3,$4,$5) ON CONFLICT (id) DO UPDATE
			SET title=$2, function=$3, module=$4, tests=$5`, i.ID, i.Title, i.Function, i.Module, i.Tests); err != nil {
			return err
		}
	}
	return nil
}

// conn waits for a pool connection up to s.wait; ok is false after
// answering the caller (paused, or no connection in time).
func (s *server) conn(w http.ResponseWriter, r *http.Request, route string) (*pgxpool.Conn, bool) {
	if s.paused.Load() {
		s.count(route, http.StatusServiceUnavailable)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.wait)
	defer cancel()
	s.waiting.Add(1)
	c, err := s.pool.Acquire(ctx)
	s.waiting.Add(-1)
	if err != nil {
		s.count(route, http.StatusServiceUnavailable)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "no database connection within "+s.wait.String(), http.StatusServiceUnavailable)
		return nil, false
	}
	return c, true
}

// slow holds the connection for the injected delay, inside Postgres, as a
// slow query would.
func (s *server) slow(ctx context.Context, c *pgxpool.Conn) error {
	if d := s.delayMs.Load(); d > 0 {
		_, err := c.Exec(ctx, "SELECT pg_sleep($1)", float64(d)/1000)
		return err
	}
	return nil
}

func (s *server) getIssue(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conn(w, r, "get_issue")
	if !ok {
		return
	}
	defer c.Release()
	ctx := r.Context()
	if err := s.slow(ctx, c); err != nil {
		s.fail(w, "get_issue", err)
		return
	}
	var out struct {
		ID       string           `json:"id"`
		Title    string           `json:"title"`
		Function string           `json:"function"`
		Module   string           `json:"module"`
		Tests    string           `json:"tests"`
		Attempts []map[string]any `json:"attempts"`
	}
	err := c.QueryRow(ctx, `SELECT id, title, function, module, tests FROM issues WHERE id = $1`, r.PathValue("id")).
		Scan(&out.ID, &out.Title, &out.Function, &out.Module, &out.Tests)
	if err != nil {
		s.count("get_issue", http.StatusNotFound)
		http.Error(w, "issue not found", http.StatusNotFound)
		return
	}
	rows, err := c.Query(ctx, `SELECT workflow, attempt, at FROM attempts WHERE issue = $1 ORDER BY at DESC LIMIT 5`, out.ID)
	if err != nil {
		s.fail(w, "get_issue", err)
		return
	}
	for rows.Next() {
		var wf string
		var n int
		var at time.Time
		if rows.Scan(&wf, &n, &at) == nil {
			out.Attempts = append(out.Attempts, map[string]any{"workflow": wf, "attempt": n, "at": at})
		}
	}
	rows.Close()
	s.count("get_issue", http.StatusOK)
	writeJSON(w, out)
}

func (s *server) postAttempt(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key      string `json:"key"`
		Workflow string `json:"workflow"`
		Issue    string `json:"issue"`
		Attempt  int    `json:"attempt"`
		Fix      string `json:"fix"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || in.Key == "" {
		s.count("post_attempt", http.StatusBadRequest)
		http.Error(w, "need a JSON body with key", http.StatusBadRequest)
		return
	}
	c, ok := s.conn(w, r, "post_attempt")
	if !ok {
		return
	}
	defer c.Release()
	ctx := r.Context()
	if err := s.slow(ctx, c); err != nil {
		s.fail(w, "post_attempt", err)
		return
	}
	tag, err := c.Exec(ctx, `INSERT INTO attempts (key, workflow, issue, attempt, fix) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (key) DO NOTHING`, in.Key, in.Workflow, in.Issue, in.Attempt, in.Fix)
	if err != nil {
		s.fail(w, "post_attempt", err)
		return
	}
	stored := tag.RowsAffected() == 1
	if stored {
		s.stored.Add(1)
	} else {
		s.duplicate.Add(1)
	}
	s.count("post_attempt", http.StatusOK)
	writeJSON(w, map[string]any{"key": in.Key, "stored": stored})
}

func (s *server) admin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DelayMs *int64 `json:"delay_ms"`
		Paused  *bool  `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if in.DelayMs != nil {
		s.delayMs.Store(*in.DelayMs)
	}
	if in.Paused != nil {
		s.paused.Store(*in.Paused)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	st := s.pool.Stat()
	fmt.Fprintf(w, "dbtool_pool_max %d\ndbtool_pool_in_use %d\ndbtool_pool_waiting %d\n", st.MaxConns(), st.AcquiredConns(), s.waiting.Load())
	fmt.Fprintf(w, "dbtool_attempts_stored_total %d\ndbtool_attempts_duplicate_total %d\n", s.stored.Load(), s.duplicate.Load())
	fmt.Fprintf(w, "dbtool_delay_ms %d\n", s.delayMs.Load())
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, n := range s.requests {
		var route string
		var status int
		fmt.Sscanf(k, "%s %d", &route, &status)
		fmt.Fprintf(w, "dbtool_requests_total{route=%q,status=\"%d\"} %d\n", route, status, n)
	}
}

func (s *server) count(route string, status int) {
	s.mu.Lock()
	s.requests[fmt.Sprintf("%s %d", route, status)]++
	s.mu.Unlock()
}

func (s *server) fail(w http.ResponseWriter, route string, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusServiceUnavailable
	}
	s.count(route, status)
	http.Error(w, err.Error(), status)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
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
