// Command agent runs one of the A2A agents in the software-maintenance team.
//
//	ROLE=coordinator  receives a task (issue, tenant), runs research, then coding
//	                  and verification for up to MAX_ROUNDS rounds, and answers with
//	                  the verified fix as a task artifact
//	ROLE=research     asks the LLM for a diagnosis
//	ROLE=coding       reads the issue from the database tool service, asks the LLM
//	                  for a fixed function, and records the attempt (a side effect)
//	ROLE=verify       runs the issue's tests against the fix in the sandbox
//
// Agents talk to each other over A2A (JSON-RPC). The specialists reach the
// constrained resources through resource.Client in RESOURCE_MODE: direct,
// queue or aquifer.
//
// Environment: ROLE, LISTEN (:9000), PUBLIC_URL; coordinator: RESEARCH_URL,
// CODING_URL, VERIFY_URL, MAX_ROUNDS (3); specialists: LLM_URL, LLM_MODEL,
// DB_URL, SANDBOX_URL, RESOURCE_MODE (direct), AQUIFER_URL, REDIS_URL,
// CALL_TIMEOUT (10m).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/agentrpc"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/corpus"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/resource"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/work"
)

func main() {
	role := os.Getenv("ROLE")
	addr := env("LISTEN", ":9000")
	public := env("PUBLIC_URL", "http://127.0.0.1"+addr)
	var h http.Handler
	switch role {
	case "coordinator":
		h = coordinator(public)
	case "research", "coding", "verify":
		a := newSpecialist()
		handlers := map[string]agentrpc.Handler{"research": a.research, "coding": a.coding, "verify": a.verify}
		h = agentrpc.Mux(role, role+" agent", public, handlers[role])
	default:
		log.Fatalf("ROLE must be coordinator, research, coding or verify")
	}
	log.Printf("%s agent on %s", role, addr)
	log.Fatal(http.ListenAndServe(addr, h))
}

// coordinator answers each task with an A2A task: submitted at once, working
// while the workflow runs, then the verified result as an artifact and
// completed (or failed). The client sees the acknowledgment apart from the
// result.
func coordinator(public string) http.Handler {
	research, coding, verify := os.Getenv("RESEARCH_URL"), os.Getenv("CODING_URL"), os.Getenv("VERIFY_URL")
	rounds := envInt("MAX_ROUNDS", 3)
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			raw, err := agentrpc.DataOf(ec.Message)
			var task work.Task
			if err == nil {
				err = json.Unmarshal(raw, &task)
			}
			if err != nil || task.Workflow == "" || task.Issue == "" {
				yield(nil, fmt.Errorf("%w: need a data part with workflow and issue", a2a.ErrInvalidParams))
				return
			}
			if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
				return
			}
			res := runWorkflow(ctx, task, research, coding, verify, rounds)
			if !yield(a2a.NewArtifactEvent(ec, a2a.NewDataPart(agentrpc.Plain(res))), nil) {
				return
			}
			state := a2a.TaskStateCompleted
			if !res.Verified {
				state = a2a.TaskStateFailed
			}
			yield(a2a.NewStatusUpdateEvent(ec, state, nil), nil)
		}
	})
	card := &a2a.AgentCard{
		Name: "coordinator", Description: "Fixes a software issue with a research, coding and verification agent, and returns a verified fix",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(public, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"application/json"}, DefaultOutputModes: []string{"application/json"},
		Capabilities: a2a.AgentCapabilities{Streaming: true},
		Skills:       []a2a.AgentSkill{{ID: "fix-issue", Name: "Fix an issue", Description: "Diagnose, fix and verify a software issue"}},
	}
	mux := http.NewServeMux()
	mux.Handle("/", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec)))
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	return mux
}

func runWorkflow(ctx context.Context, t work.Task, researchURL, codingURL, verifyURL string, maxRounds int) work.Result {
	res := work.Result{Workflow: t.Workflow, Issue: t.Issue, Tenant: t.Tenant}
	var r work.Research
	if err := agentrpc.Call(ctx, researchURL, work.Step{Workflow: t.Workflow, Issue: t.Issue, Tenant: t.Tenant, Round: 1}, &r); err != nil {
		res.Error = "research: " + err.Error()
		return res
	}
	res.Calls = append(res.Calls, r.Calls...)
	testOutput := ""
	for round := 1; round <= maxRounds; round++ {
		res.Rounds = round
		var c work.Coding
		if err := agentrpc.Call(ctx, codingURL, work.Step{Workflow: t.Workflow, Issue: t.Issue, Tenant: t.Tenant, Round: round,
			Diagnosis: r.Diagnosis, TestOutput: testOutput}, &c); err != nil {
			res.Error = "coding: " + err.Error()
			return res
		}
		res.Calls = append(res.Calls, c.Calls...)
		var v work.Verify
		if err := agentrpc.Call(ctx, verifyURL, work.Step{Workflow: t.Workflow, Issue: t.Issue, Tenant: t.Tenant, Round: round, Fix: c.Fix}, &v); err != nil {
			res.Error = "verify: " + err.Error()
			return res
		}
		res.Calls = append(res.Calls, v.Calls...)
		res.Fix, res.ExecutionID = c.Fix, v.ExecutionID
		if v.Passed {
			res.Verified = true
			return res
		}
		testOutput = v.Output
	}
	res.Error = fmt.Sprintf("no passing fix in %d rounds", maxRounds)
	return res
}

// specialist holds what the research, coding and verify agents share.
type specialist struct {
	rc                           *resource.Client
	llmURL, model, dbURL, sbxURL string
	timeout                      time.Duration
}

func newSpecialist() *specialist {
	timeout, err := time.ParseDuration(env("CALL_TIMEOUT", "10m"))
	if err != nil {
		log.Fatalf("CALL_TIMEOUT: %v", err)
	}
	rc := &resource.Client{Mode: env("RESOURCE_MODE", "direct"), AquiferURL: os.Getenv("AQUIFER_URL"),
		HTTP:        &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}},
		BaseBackoff: 200 * time.Millisecond, MaxBackoff: 10 * time.Second}
	if rc.Mode == "queue" {
		q, err := resource.NewRedisQueue(env("REDIS_URL", "redis://127.0.0.1:6379"))
		if err != nil {
			log.Fatalf("REDIS_URL: %v", err)
		}
		rc.Queue = q
	}
	return &specialist{rc: rc, llmURL: os.Getenv("LLM_URL"), model: env("LLM_MODEL", "stand-in"),
		dbURL: os.Getenv("DB_URL"), sbxURL: os.Getenv("SANDBOX_URL"), timeout: timeout}
}

// call makes one resource call and records it.
func (s *specialist) call(ctx context.Context, c resource.Call, step string) (resource.Result, work.CallRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	start := time.Now()
	r, err := s.rc.Do(ctx, c)
	rec := work.CallRecord{Resource: c.Resource, Step: step, Key: c.Key, Status: r.Status, Attempts: r.Attempts,
		BackoffMs: r.Backoff.Milliseconds(), Ms: time.Since(start).Milliseconds()}
	if err == nil && (r.Status < 200 || r.Status > 299) {
		err = fmt.Errorf("%s answered %d: %.200s", c.Resource, r.Status, r.Body)
	}
	return r, rec, err
}

// llm asks the model; the tag lets the stand-in model answer from the corpus
// and is harmless to a real model.
func (s *specialist) llm(ctx context.Context, st work.Step, step, system, user string) (string, work.CallRecord, error) {
	body, _ := json.Marshal(map[string]any{
		"model": s.model, "temperature": 0, "max_tokens": 400,
		"messages": []map[string]string{
			{"role": "system", "content": fmt.Sprintf("%s [issue=%s step=%s attempt=%d]", system, st.Issue, step, st.Round)},
			{"role": "user", "content": user},
		},
	})
	key := fmt.Sprintf("%s-%s-%d", st.Workflow, step, st.Round)
	r, rec, err := s.call(ctx, resource.Call{Resource: "llm", Method: http.MethodPost, URL: s.llmURL + "/v1/chat/completions",
		Body: body, Key: key, Tenant: st.Tenant}, step)
	if err != nil {
		return "", rec, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(r.Body, &out); err != nil || len(out.Choices) == 0 {
		return "", rec, fmt.Errorf("llm answer unreadable: %.200s", r.Body)
	}
	rec.Tokens = out.Usage.CompletionTokens
	return out.Choices[0].Message.Content, rec, nil
}

func (s *specialist) research(ctx context.Context, raw json.RawMessage) (any, error) {
	var st work.Step
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	issue, err := corpus.Get(st.Issue)
	if err != nil {
		return nil, err
	}
	text, rec, err := s.llm(ctx, st, "research", "You are a careful software engineer. Diagnose the bug in one short paragraph.",
		fmt.Sprintf("Issue: %s\n\nModule:\n%s\nFailing test:\n%s", issue.Title, issue.Module, issue.Tests))
	if err != nil {
		return nil, err
	}
	return work.Research{Diagnosis: text, Calls: []work.CallRecord{rec}}, nil
}

func (s *specialist) coding(ctx context.Context, raw json.RawMessage) (any, error) {
	var st work.Step
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	var calls []work.CallRecord
	// Read the issue and its history from the database.
	r, rec, err := s.call(ctx, resource.Call{Resource: "db", Method: http.MethodGet, URL: s.dbURL + "/issues/" + st.Issue,
		Key: fmt.Sprintf("%s-read-%d", st.Workflow, st.Round), Tenant: st.Tenant}, "read-issue")
	calls = append(calls, rec)
	if err != nil {
		return nil, err
	}
	var issue struct {
		Title, Function, Module, Tests string
	}
	if err := json.Unmarshal(r.Body, &issue); err != nil {
		return nil, fmt.Errorf("issue unreadable: %w", err)
	}
	prompt := fmt.Sprintf("Issue: %s\nDiagnosis: %s\n\nModule:\n%s\nTests:\n%s\n", issue.Title, st.Diagnosis, issue.Module, issue.Tests)
	if st.TestOutput != "" {
		prompt += "\nYour previous fix failed with:\n" + st.TestOutput + "\n"
	}
	prompt += "\nReply with the corrected " + issue.Function + " function only, in a ```python block."
	text, rec, err := s.llm(ctx, st, "code", "You are a careful software engineer who writes minimal, correct fixes.", prompt)
	calls = append(calls, rec)
	if err != nil {
		return nil, err
	}
	fix := corpus.ExtractFunction(text)
	// Record the attempt: a side effect, stored once per key.
	body, _ := json.Marshal(map[string]any{"key": fmt.Sprintf("%s-attempt-%d", st.Workflow, st.Round), "workflow": st.Workflow,
		"issue": st.Issue, "attempt": st.Round, "fix": fix})
	_, rec, err = s.call(ctx, resource.Call{Resource: "db", Method: http.MethodPost, URL: s.dbURL + "/attempts", Body: body,
		Key: fmt.Sprintf("%s-attempt-%d", st.Workflow, st.Round), Tenant: st.Tenant}, "record-attempt")
	calls = append(calls, rec)
	if err != nil {
		return nil, err
	}
	return work.Coding{Fix: fix, Calls: calls}, nil
}

func (s *specialist) verify(ctx context.Context, raw json.RawMessage) (any, error) {
	var st work.Step
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if st.Fix == "" {
		return nil, errors.New("verify needs a fix")
	}
	key := fmt.Sprintf("%s-verify-%d", st.Workflow, st.Round)
	body, _ := json.Marshal(map[string]any{"key": key, "workflow": st.Workflow, "issue": st.Issue, "fix": st.Fix})
	r, rec, err := s.call(ctx, resource.Call{Resource: "sandbox", Method: http.MethodPost, URL: s.sbxURL + "/run", Body: body,
		Key: key, Tenant: st.Tenant}, "run-tests")
	if err != nil {
		return nil, err
	}
	var ex struct {
		ExecutionID string `json:"execution_id"`
		Passed      bool   `json:"passed"`
		Output      string `json:"output"`
	}
	if err := json.Unmarshal(r.Body, &ex); err != nil {
		return nil, fmt.Errorf("sandbox answer unreadable: %w", err)
	}
	return work.Verify{Passed: ex.Passed, ExecutionID: ex.ExecutionID, Output: ex.Output, Calls: []work.CallRecord{rec}}, nil
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}
