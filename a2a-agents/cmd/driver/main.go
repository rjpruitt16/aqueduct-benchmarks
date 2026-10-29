// Command driver submits workflows to the coordinator over A2A and keeps an
// independent ledger of every one, from submission to a verified result.
//
// A workflow counts as completed only when the coordinator's artifact says
// verified AND the sandbox's own record of that execution (GET
// /executions/{id}) shows the issue's tests passed for this workflow. The
// A2A acknowledgment (the task's first event), the result and the
// verification are timed separately.
//
// Load: PHASES (name:seconds:workflows-per-second,...) spread across TENANTS
// (name:weight,...); or COUNT workflows submitted at once (smoke tests).
//
// Environment: COORDINATOR_URL, SANDBOX_URL, PHASES or COUNT, TENANTS
// (big:1), DRAIN (10m), OUT (out), METRICS (name=url,... scraped at the end).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/agentrpc"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/corpus"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/work"
)

type entry struct {
	Workflow  string            `json:"workflow"`
	Tenant    string            `json:"tenant"`
	Issue     string            `json:"issue"`
	Phase     string            `json:"phase"`
	Submitted time.Time         `json:"submitted"`
	AckMs     int64             `json:"ack_ms"`  // first A2A event: the coordinator has the task
	EndMs     int64             `json:"end_ms"`  // the final event
	State     string            `json:"state"`   // the A2A task's final state
	Outcome   string            `json:"outcome"` // completed (verified), failed, unverified, unfinished, error
	Rounds    int               `json:"rounds"`
	Fix       string            `json:"fix,omitempty"` // the final fix, verified or not
	Error     string            `json:"error,omitempty"`
	Calls     []work.CallRecord `json:"calls,omitempty"`
}

func main() {
	coord := os.Getenv("COORDINATOR_URL")
	sandbox := os.Getenv("SANDBOX_URL")
	out := env("OUT", "out")
	os.MkdirAll(out, 0o755)
	tenants := parseTenants(env("TENANTS", "big:1"))
	drain, _ := time.ParseDuration(env("DRAIN", "10m"))
	issues := corpus.All()

	var mu sync.Mutex
	var ledger []*entry
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq := 0
	runID := fmt.Sprintf("w%x", time.Now().Unix())
	submit := func(phase, tenant string) {
		seq++
		e := &entry{Workflow: fmt.Sprintf("%s-%d", runID, seq), Tenant: tenant, Issue: issues[(seq-1)%len(issues)].ID,
			Phase: phase, Submitted: time.Now()}
		mu.Lock()
		ledger = append(ledger, e)
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(ctx, coord, sandbox, e)
		}()
	}

	if n, err := strconv.Atoi(os.Getenv("COUNT")); err == nil && n > 0 {
		for i := 0; i < n; i++ {
			submit("smoke", pick(tenants, i))
		}
	} else {
		for _, ph := range parsePhases(env("PHASES", "steady:60:0.5")) {
			log.Printf("phase %s: %.2f workflows/s for %s", ph.name, ph.rate, ph.length)
			end := time.Now().Add(ph.length)
			owed, i := 0.0, 0
			for time.Now().Before(end) {
				time.Sleep(100 * time.Millisecond)
				owed += ph.rate / 10
				for ; owed >= 1; owed-- {
					submit(ph.name, pick(tenants, i))
					i++
				}
			}
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(drain):
		log.Printf("drain of %s ended with workflows unfinished", drain)
		cancel()
		<-done
	}

	mu.Lock()
	defer mu.Unlock()
	f, _ := os.Create(filepath.Join(out, "ledger.jsonl"))
	enc := json.NewEncoder(f)
	for _, e := range ledger {
		enc.Encode(e)
	}
	f.Close()
	sum := summarize(ledger)
	b, _ := json.MarshalIndent(sum, "", "  ")
	os.WriteFile(filepath.Join(out, "summary.json"), b, 0o644)
	fmt.Println(string(b))
	scrapeMetrics(env("METRICS", ""), filepath.Join(out, "metrics_end.txt"))
}

// run submits one workflow over A2A streaming and records what came back.
func run(ctx context.Context, coord, sandbox string, e *entry) {
	e.Outcome = "unfinished"
	c, err := agentrpc.Client(ctx, coord)
	if err != nil {
		e.Outcome, e.Error = "error", err.Error()
		return
	}
	task := work.Task{Workflow: e.Workflow, Issue: e.Issue, Tenant: e.Tenant}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(agentrpc.Plain(task)))
	var res *work.Result
	for ev, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg, Tenant: e.Tenant}) {
		if err != nil {
			e.Outcome, e.Error = "error", err.Error()
			e.EndMs = time.Since(e.Submitted).Milliseconds()
			return
		}
		if e.AckMs == 0 {
			e.AckMs = max(time.Since(e.Submitted).Milliseconds(), 1)
		}
		switch v := ev.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			for _, p := range v.Artifact.Parts {
				if d := p.Data(); d != nil {
					raw, _ := json.Marshal(d)
					var r work.Result
					if json.Unmarshal(raw, &r) == nil {
						res = &r
					}
				}
			}
		case *a2a.TaskStatusUpdateEvent:
			if v.Status.State.Terminal() {
				e.State = string(v.Status.State)
			}
		case *a2a.Task:
			if v.Status.State.Terminal() {
				e.State = string(v.Status.State)
			}
		}
	}
	e.EndMs = time.Since(e.Submitted).Milliseconds()
	if res == nil {
		e.Outcome = "error"
		if e.Error == "" {
			e.Error = "no result artifact"
		}
		return
	}
	e.Rounds, e.Calls, e.Error, e.Fix = res.Rounds, res.Calls, res.Error, res.Fix
	if !res.Verified {
		e.Outcome = "failed"
		return
	}
	// Check the claim against the sandbox's own record of the execution.
	if verifiedBySandbox(ctx, sandbox, res.ExecutionID, e.Workflow) {
		e.Outcome = "completed"
	} else {
		e.Outcome = "unverified"
	}
}

func verifiedBySandbox(ctx context.Context, sandbox, executionID, workflow string) bool {
	if executionID == "" {
		return false
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, sandbox+"/executions/"+executionID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var ex struct {
		Workflow string `json:"workflow"`
		Passed   bool   `json:"passed"`
	}
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&ex) == nil && ex.Passed && ex.Workflow == workflow
}

type tenantSummary struct {
	Submitted   int            `json:"submitted"`
	Outcomes    map[string]int `json:"outcomes"`
	P50Ms       float64        `json:"p50_ms"`
	P95Ms       float64        `json:"p95_ms"`
	P99Ms       float64        `json:"p99_ms"`
	AckP99Ms    float64        `json:"ack_p99_ms"`
	ResourceReq map[string]int `json:"requests_by_resource"` // requests the agents sent, all attempts
	BackoffMs   int64          `json:"backoff_ms"`
}

func summarize(ledger []*entry) map[string]any {
	byTenant := map[string]*tenantSummary{}
	lat, ack := map[string][]float64{}, map[string][]float64{}
	outcomes := map[string]int{}
	var all []float64
	completed := 0
	requests := 0
	for _, e := range ledger {
		t := byTenant[e.Tenant]
		if t == nil {
			t = &tenantSummary{Outcomes: map[string]int{}, ResourceReq: map[string]int{}}
			byTenant[e.Tenant] = t
		}
		t.Submitted++
		t.Outcomes[e.Outcome]++
		outcomes[e.Outcome]++
		for _, c := range e.Calls {
			t.ResourceReq[c.Resource] += c.Attempts
			t.BackoffMs += c.BackoffMs
			requests += c.Attempts
		}
		if e.AckMs > 0 {
			ack[e.Tenant] = append(ack[e.Tenant], float64(e.AckMs))
		}
		if e.Outcome == "completed" {
			completed++
			lat[e.Tenant] = append(lat[e.Tenant], float64(e.EndMs))
			all = append(all, float64(e.EndMs))
		}
	}
	for name, t := range byTenant {
		t.P50Ms, t.P95Ms, t.P99Ms = pct(lat[name], 0.5), pct(lat[name], 0.95), pct(lat[name], 0.99)
		t.AckP99Ms = pct(ack[name], 0.99)
	}
	perWorkflow := 0.0
	if completed > 0 {
		perWorkflow = float64(requests) / float64(completed)
	}
	return map[string]any{
		"workflows": len(ledger), "outcomes": outcomes,
		"p50_ms": pct(all, 0.5), "p95_ms": pct(all, 0.95), "p99_ms": pct(all, 0.99),
		"resource_requests_per_completed_workflow": math.Round(perWorkflow*100) / 100,
		"tenants": byTenant,
	}
}

func scrapeMetrics(spec, path string) {
	if spec == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	for _, kv := range strings.Split(spec, ",") {
		name, url, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		resp, err := http.Get(url)
		if err != nil {
			fmt.Fprintf(f, "# %s: %v\n", name, err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		fmt.Fprintf(f, "# %s\n%s\n", name, b)
	}
}

func pct(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[int(math.Min(float64(len(s)-1), q*float64(len(s))))]
}

type phase struct {
	name   string
	length time.Duration
	rate   float64
}

func parsePhases(spec string) []phase {
	var out []phase
	for _, p := range strings.Split(spec, ",") {
		f := strings.Split(p, ":")
		if len(f) != 3 {
			log.Fatalf("PHASES: %q is not name:seconds:rate", p)
		}
		secs, _ := strconv.ParseFloat(f[1], 64)
		rate, _ := strconv.ParseFloat(f[2], 64)
		out = append(out, phase{f[0], time.Duration(secs * float64(time.Second)), rate})
	}
	return out
}

type tenantWeight struct {
	name   string
	weight int
}

func parseTenants(spec string) []tenantWeight {
	var out []tenantWeight
	for _, p := range strings.Split(spec, ",") {
		name, w, _ := strings.Cut(p, ":")
		n, err := strconv.Atoi(w)
		if err != nil || n < 1 {
			n = 1
		}
		out = append(out, tenantWeight{name, n})
	}
	return out
}

// pick spreads submissions across tenants in proportion to their weights.
func pick(ts []tenantWeight, i int) string {
	total := 0
	for _, t := range ts {
		total += t.weight
	}
	k := i % total
	for _, t := range ts {
		if k < t.weight {
			return t.name
		}
		k -= t.weight
	}
	return ts[0].name
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
