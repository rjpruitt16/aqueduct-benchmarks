// Command sandbox runs an issue's tests against a proposed fix, in isolation,
// with a fixed number of concurrent executions.
//
//	POST /run               {key, workflow, issue, fix}: run the issue's tests against
//	                        its module with fix applied; 429 + Retry-After when every slot is busy
//	GET  /executions/{id}   one execution's record, so a result can be checked against
//	                        what actually ran
//	POST /admin             {"extra_ms": N} holds each slot N ms longer (slower tests)
//	GET  /metrics           slots, executions, refusals, keys executed more than once
//
// Every execution is a fresh, isolated process: RUNNER=docker (local) runs a
// container with no network, a read-only root, a private /tmp and limits on
// memory, CPU and processes; RUNNER=nsjail (Fly) does the same with nsjail.
// The code is model-written and never trusted. Executions are not
// deduplicated: the sandbox runs what it's asked to, and logs each run with
// its key, so repeats are counted where they happen.
//
// Environment: LISTEN (:8200), SLOTS (4), EXEC_TIMEOUT (15s), RUNNER (docker),
// IMAGE (python:3.12-alpine), LOG (executions.jsonl).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/corpus"
)

type execution struct {
	ID       string    `json:"execution_id"`
	Key      string    `json:"key"`
	Workflow string    `json:"workflow"`
	Issue    string    `json:"issue"`
	Passed   bool      `json:"passed"`
	Output   string    `json:"output"`
	Started  time.Time `json:"started"`
	Millis   int64     `json:"ms"`
}

type server struct {
	slots   chan struct{}
	timeout time.Duration
	runner  string
	image   string
	extraMs atomic.Int64
	seq     atomic.Int64
	refused atomic.Int64

	mu     sync.Mutex
	byID   map[string]execution
	byKey  map[string]int
	logF   *os.File
	logEnc *json.Encoder
}

func main() {
	timeout, err := time.ParseDuration(envStr("EXEC_TIMEOUT", "15s"))
	if err != nil {
		log.Fatalf("EXEC_TIMEOUT: %v", err)
	}
	f, err := os.OpenFile(envStr("LOG", "executions.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{slots: make(chan struct{}, envInt("SLOTS", 4)), timeout: timeout,
		runner: envStr("RUNNER", "docker"), image: envStr("IMAGE", "python:3.12-alpine"),
		byID: map[string]execution{}, byKey: map[string]int{}, logF: f, logEnc: json.NewEncoder(f)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", s.run)
	mux.HandleFunc("GET /executions/{id}", s.get)
	mux.HandleFunc("POST /admin", s.admin)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	addr := envStr("LISTEN", ":8200")
	log.Printf("sandbox on %s: %d slots, runner %s", addr, cap(s.slots), s.runner)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *server) run(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key      string `json:"key"`
		Workflow string `json:"workflow"`
		Issue    string `json:"issue"`
		Fix      string `json:"fix"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	issue, err := corpus.Get(in.Issue)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case s.slots <- struct{}{}:
	default:
		s.refused.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "every sandbox slot is busy", http.StatusTooManyRequests)
		return
	}
	defer func() { <-s.slots }()

	ex := execution{ID: fmt.Sprintf("x%d-%d", time.Now().Unix(), s.seq.Add(1)), Key: in.Key, Workflow: in.Workflow,
		Issue: in.Issue, Started: time.Now().UTC()}
	if d := s.extraMs.Load(); d > 0 {
		time.Sleep(time.Duration(d) * time.Millisecond)
	}
	ex.Passed, ex.Output = s.execute(r.Context(), ex.ID, corpus.Apply(issue.Module, issue.Function, in.Fix), issue.Tests)
	ex.Millis = time.Since(ex.Started).Milliseconds()

	s.mu.Lock()
	s.byID[ex.ID] = ex
	s.byKey[ex.Key]++
	s.logEnc.Encode(ex)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ex)
}

// execute runs tests against module in a fresh isolated process and reports
// whether they passed, with their output (truncated).
func (s *server) execute(ctx context.Context, id, module, tests string) (bool, string) {
	dir, err := os.MkdirTemp("", "sbx-")
	if err != nil {
		return false, err.Error()
	}
	defer os.RemoveAll(dir)
	os.Chmod(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "module.py"), []byte(module), 0o644); err != nil {
		return false, err.Error()
	}
	if err := os.WriteFile(filepath.Join(dir, "test_issue.py"), []byte(tests), 0o644); err != nil {
		return false, err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch s.runner {
	case "docker":
		name := "sbx-" + id
		cmd = exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name, "--network", "none",
			"--read-only", "--tmpfs", "/tmp:size=16m", "--memory", "128m", "--cpus", "0.5", "--pids-limit", "64",
			"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--user", "65534:65534",
			"-v", dir+":/work:ro", "-w", "/work", s.image, "python", "-B", "test_issue.py")
		defer exec.Command("docker", "rm", "-f", name).Run()
	case "nsjail":
		cmd = exec.CommandContext(ctx, "nsjail", "--mode", "o", "--quiet", "--disable_proc",
			"--rlimit_as", "256", "--rlimit_cpu", "10", "--rlimit_nproc", "64", "--time_limit", strconv.Itoa(int(s.timeout.Seconds())),
			"--user", "65534", "--group", "65534", "--chroot", "/", "--bindmount_ro", dir+":/work", "--cwd", "/work",
			"--", "/usr/bin/python3", "-B", "test_issue.py")
	default:
		return false, "no isolated runner configured; refusing to execute"
	}
	out, err := cmd.CombinedOutput()
	if len(out) > 2000 {
		out = out[len(out)-2000:]
	}
	return err == nil, string(out)
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ex, ok := s.byID[r.PathValue("id")]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "no such execution", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ex)
}

func (s *server) admin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExtraMs *int64 `json:"extra_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if in.ExtraMs != nil {
		s.extraMs.Store(*in.ExtraMs)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repeated := 0
	for _, n := range s.byKey {
		if n > 1 {
			repeated += n - 1
		}
	}
	fmt.Fprintf(w, "sandbox_slots %d\nsandbox_slots_busy %d\nsandbox_executions_total %d\n", cap(s.slots), len(s.slots), len(s.byID))
	fmt.Fprintf(w, "sandbox_refused_total %d\nsandbox_repeated_executions_total %d\n", s.refused.Load(), repeated)
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
