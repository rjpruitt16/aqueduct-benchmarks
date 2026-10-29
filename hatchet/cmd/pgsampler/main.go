// Command pgsampler records Postgres load during a benchmark run: every
// interval it writes one JSON line with connections by state, waiting
// sessions, lock waits, transaction and row rates, cache hit ratio, database
// size and the size of Hatchet's busiest tables. On exit it writes the top
// pg_stat_statements entries for the run.
//
// Environment:
//
//	DATABASE_URL  Postgres connection string (needs pg_stat_statements loaded)
//	INTERVAL      sampling interval (default 10s)
//	OUT           output directory (default ./out)
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// tables are Hatchet's high-churn tables whose growth shows backlog and bloat.
var tables = []string{"v1_queue_item", "v1_task", "v1_task_runtime", "v1_concurrency_slot", "v1_task_event", "v1_tasks_olap", "v1_task_events_olap", "message_queue_item"}

type sample struct {
	At           time.Time         `json:"at"`
	Connections  map[string]int    `json:"connections_by_state"`
	Waiting      map[string]int    `json:"waiting_by_event_type"`
	LockWaits    int               `json:"lock_waits"`
	CommitsPerS  float64           `json:"commits_per_s"`
	RollbackPerS float64           `json:"rollbacks_per_s"`
	TupWrittenPS float64           `json:"rows_written_per_s"`
	TupReadPerS  float64           `json:"rows_read_per_s"`
	CacheHit     float64           `json:"cache_hit_ratio"`
	DBBytes      int64             `json:"db_bytes"`
	TableBytes   map[string]int64  `json:"table_bytes"`
	TableRows    map[string]int64  `json:"table_live_rows"`
	Errors       map[string]string `json:"errors,omitempty"`
}

type dbCounters struct {
	at                                time.Time
	commits, rollbacks, written, read int64
	blksHit, blksRead                 int64
}

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is required")
	}
	every := 10 * time.Second
	if d, err := time.ParseDuration(os.Getenv("INTERVAL")); err == nil && d > 0 {
		every = d
	}
	out := os.Getenv("OUT")
	if out == "" {
		out = "out"
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := connect(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	// A query that times out closes the connection, which is most likely
	// while the database is overloaded: exactly what this records. Reconnect
	// before each sample instead of losing the rest of the run.
	defer func() { conn.Close(context.Background()) }()
	ensure := func(c context.Context) bool {
		if !conn.IsClosed() {
			return true
		}
		fresh, err := pgx.Connect(c, url)
		if err != nil {
			return false
		}
		conn = fresh
		return true
	}
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		log.Printf("pg_stat_statements unavailable: %v", err)
	} else if _, err := conn.Exec(ctx, "SELECT pg_stat_statements_reset()"); err != nil {
		log.Printf("pg_stat_statements_reset: %v", err)
	}

	f, err := os.Create(filepath.Join(out, "postgres.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	prev, _ := readCounters(ctx, conn)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if ensure(final) {
				writeTopStatements(conn, filepath.Join(out, "statements.json"))
				writeDuplicates(conn, filepath.Join(out, "duplicates.json"))
			}
			cancel()
			return
		case <-ticker.C:
		}
		if !ensure(ctx) {
			_ = enc.Encode(sample{At: time.Now().UTC(), Errors: map[string]string{"connect": "reconnect failed"}})
			continue
		}
		s, cur := take(ctx, conn, prev)
		prev = cur
		_ = enc.Encode(s)
	}
}

func connect(ctx context.Context, url string) (*pgx.Conn, error) {
	var lastErr error
	for i := 0; i < 30; i++ {
		c, err := pgx.Connect(ctx, url)
		if err == nil {
			return c, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}

func readCounters(ctx context.Context, conn *pgx.Conn) (dbCounters, error) {
	c := dbCounters{at: time.Now()}
	err := conn.QueryRow(ctx, `
		SELECT xact_commit, xact_rollback, tup_inserted + tup_updated + tup_deleted,
		       tup_returned + tup_fetched, blks_hit, blks_read
		FROM pg_stat_database WHERE datname = current_database()`).
		Scan(&c.commits, &c.rollbacks, &c.written, &c.read, &c.blksHit, &c.blksRead)
	return c, err
}

// take reads one sample. A failed query is recorded in Errors rather than
// stopping the run: an overloaded database answering slowly is a result.
func take(ctx context.Context, conn *pgx.Conn, prev dbCounters) (sample, dbCounters) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s := sample{At: time.Now().UTC(), Connections: map[string]int{}, Waiting: map[string]int{},
		TableBytes: map[string]int64{}, TableRows: map[string]int64{}, Errors: map[string]string{}}

	if rows, err := conn.Query(qctx, `
		SELECT coalesce(state, 'unknown'), count(*) FROM pg_stat_activity
		WHERE datname = current_database() GROUP BY 1`); err != nil {
		s.Errors["activity"] = err.Error()
	} else {
		for rows.Next() {
			var state string
			var n int
			if rows.Scan(&state, &n) == nil {
				s.Connections[state] = n
			}
		}
		rows.Close()
	}
	if rows, err := conn.Query(qctx, `
		SELECT wait_event_type, count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type IS NOT NULL AND state = 'active' GROUP BY 1`); err != nil {
		s.Errors["waits"] = err.Error()
	} else {
		for rows.Next() {
			var kind string
			var n int
			if rows.Scan(&kind, &n) == nil {
				s.Waiting[kind] = n
			}
		}
		rows.Close()
	}
	if err := conn.QueryRow(qctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&s.LockWaits); err != nil {
		s.Errors["locks"] = err.Error()
	}
	if err := conn.QueryRow(qctx, `SELECT pg_database_size(current_database())`).Scan(&s.DBBytes); err != nil {
		s.Errors["db_size"] = err.Error()
	}
	if rows, err := conn.Query(qctx, `
		SELECT c.relname, pg_total_relation_size(c.oid), coalesce(st.n_live_tup, 0)
		FROM pg_class c LEFT JOIN pg_stat_user_tables st ON st.relid = c.oid
		WHERE c.relname = ANY($1) AND c.relkind IN ('r', 'p')`, tables); err != nil {
		s.Errors["tables"] = err.Error()
	} else {
		for rows.Next() {
			var name string
			var size, live int64
			if rows.Scan(&name, &size, &live) == nil {
				s.TableBytes[name], s.TableRows[name] = size, live
			}
		}
		rows.Close()
	}

	cur, err := readCounters(qctx, conn)
	if err != nil {
		s.Errors["counters"] = err.Error()
		return s, prev
	}
	if secs := cur.at.Sub(prev.at).Seconds(); secs > 0 && !prev.at.IsZero() {
		s.CommitsPerS = float64(cur.commits-prev.commits) / secs
		s.RollbackPerS = float64(cur.rollbacks-prev.rollbacks) / secs
		s.TupWrittenPS = float64(cur.written-prev.written) / secs
		s.TupReadPerS = float64(cur.read-prev.read) / secs
		if hit, read := cur.blksHit-prev.blksHit, cur.blksRead-prev.blksRead; hit+read > 0 {
			s.CacheHit = float64(hit) / float64(hit+read)
		}
	}
	if len(s.Errors) == 0 {
		s.Errors = nil
	}
	return s, cur
}

// writeTopStatements saves the 30 statements with the most total execution
// time since the run started.
// writeDuplicates counts, from Hatchet's own records, documents it ran as
// more than one workflow run: the backend's view of duplicate executions,
// whatever any caller or gateway believed. One v1_dags_olap row is one
// workflow run, keyed by the doc_id the driver puts in its metadata.
func writeDuplicates(conn *pgx.Conn, path string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var docs, dupDocs, extra int64
	err := conn.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE n > 1), coalesce(sum(n - 1) FILTER (WHERE n > 1), 0)
		FROM (SELECT additional_metadata->>'doc_id' AS doc, count(*) AS n FROM v1_dags_olap
		      WHERE additional_metadata ? 'doc_id' GROUP BY 1) runs`).Scan(&docs, &dupDocs, &extra)
	out := map[string]any{"documents": docs, "documents_with_more_than_one_run": dupDocs, "extra_runs": extra}
	if err != nil {
		out = map[string]any{"error": err.Error()}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Printf("write duplicates: %v", err)
	}
}

func writeTopStatements(conn *pgx.Conn, path string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := conn.Query(ctx, `
		SELECT left(query, 300), calls, total_exec_time, mean_exec_time, rows
		FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		ORDER BY total_exec_time DESC LIMIT 30`)
	if err != nil {
		log.Printf("pg_stat_statements: %v", err)
		return
	}
	defer rows.Close()
	type stmt struct {
		Query   string  `json:"query"`
		Calls   int64   `json:"calls"`
		TotalMs float64 `json:"total_ms"`
		MeanMs  float64 `json:"mean_ms"`
		Rows    int64   `json:"rows"`
	}
	var out []stmt
	for rows.Next() {
		var s stmt
		if rows.Scan(&s.Query, &s.Calls, &s.TotalMs, &s.MeanMs, &s.Rows) == nil {
			out = append(out, s)
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Printf("write %s: %v", path, err)
	}
}
