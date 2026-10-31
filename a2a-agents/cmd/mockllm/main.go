// Command mockllm is the stand-in model for development, smoke tests and
// calibration: an OpenAI-compatible chat-completions server that answers from
// the corpus deterministically, and behaves like one GPU running vLLM.
//
// Behavior:
//   - Answers: an agent's system prompt carries a tag, [issue=<id> step=<research|code> attempt=<n>].
//     research gets a diagnosis; code gets the corpus fix in a fenced block,
//     except on attempt 1 of an issue with a wrong fix, which gets the wrong one.
//   - Capacity: at most MAX_SEQS requests generate at once, each at
//     TOKENS_PER_S tokens/s after PREFILL_MS; the rest wait in arrival order.
//     With MAX_WAITING > 0, a request beyond that many waiting gets a 503.
//   - Metrics on /metrics use vLLM's names: vllm:num_requests_running,
//     vllm:num_requests_waiting, vllm:generation_tokens_total, vllm:prompt_tokens_total.
//   - POST /admin {"max_seqs":N,"tokens_per_s":R} changes capacity live (the
//     recovery scenario).
//
// Environment: LISTEN (default :8000), MAX_SEQS (8), TOKENS_PER_S (40),
// PREFILL_MS (50), MAX_WAITING (0: unlimited), RESEARCH_TOKENS (150), CODE_TOKENS (100).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/advertise"
	"github.com/rjpruitt16/aqueduct-benchmarks/a2a-agents/internal/corpus"
)

var tag = regexp.MustCompile(`\[issue=([^ \]]+) step=([^ \]]+) attempt=(\d+)\]`)

type gpu struct {
	mu         sync.Mutex
	cond       *sync.Cond
	maxSeqs    int
	tokensPerS float64
	running    int
	waiting    int
	maxWaiting int
	nextTicket int64 // arrival order
	serving    int64 // next ticket allowed to start
	genTokens  int64
	promptToks int64
	requests   map[string]int64
}

func (g *gpu) admit() (ticket int64, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.maxWaiting > 0 && g.waiting >= g.maxWaiting && g.running >= g.maxSeqs {
		return 0, false
	}
	ticket = g.nextTicket
	g.nextTicket++
	g.waiting++
	for g.running >= g.maxSeqs || ticket != g.serving {
		g.cond.Wait()
	}
	g.serving++
	g.waiting--
	g.running++
	g.cond.Broadcast()
	return ticket, true
}

func (g *gpu) release(gen, prompt int) {
	g.mu.Lock()
	g.running--
	g.genTokens += int64(gen)
	g.promptToks += int64(prompt)
	g.cond.Broadcast()
	g.mu.Unlock()
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func main() {
	g := &gpu{maxSeqs: envInt("MAX_SEQS", 8), tokensPerS: float64(envInt("TOKENS_PER_S", 40)),
		maxWaiting: envInt("MAX_WAITING", 0), requests: map[string]int64{}}
	g.cond = sync.NewCond(&g.mu)
	prefill := time.Duration(envInt("PREFILL_MS", 50)) * time.Millisecond
	researchTokens, codeTokens := envInt("RESEARCH_TOKENS", 150), envInt("CODE_TOKENS", 100)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string    `json:"model"`
			Messages []message `json:"messages"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		prompt := ""
		for _, m := range req.Messages {
			prompt += m.Content + "\n"
		}
		m := tag.FindStringSubmatch(prompt)
		if m == nil {
			http.Error(w, "no [issue=… step=… attempt=…] tag in the prompt", http.StatusBadRequest)
			return
		}
		issue, err := corpus.Get(m[1])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		attempt, _ := strconv.Atoi(m[3])
		var content string
		tokens := researchTokens
		switch m[2] {
		case "research":
			content = fmt.Sprintf("Diagnosis for %s: %s. The function %s() needs a one-line correction; the failing assertion shows the expected behavior.", issue.ID, issue.Title, issue.Function)
		case "code":
			fix := issue.Fix
			if attempt == 1 && issue.NeedsSecondRound() {
				fix = issue.WrongFix
			}
			content = "Proposed fix:\n```python\n" + fix + "```\n"
			tokens = codeTokens
		default:
			http.Error(w, "unknown step "+m[2], http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.requests[m[2]]++
		g.mu.Unlock()
		if _, ok := g.admit(); !ok {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests waiting", http.StatusServiceUnavailable)
			return
		}
		g.mu.Lock()
		rate := g.tokensPerS
		g.mu.Unlock()
		promptTokens := len(prompt) / 4
		time.Sleep(prefill + time.Duration(float64(tokens)/rate*float64(time.Second)))
		g.release(tokens, promptTokens)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": fmt.Sprintf("mock-%d", time.Now().UnixNano()), "object": "chat.completion", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "message": message{Role: "assistant", Content: content}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": promptTokens, "completion_tokens": tokens, "total_tokens": promptTokens + tokens},
		})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		fmt.Fprintf(w, "vllm:num_requests_running %d\nvllm:num_requests_waiting %d\n", g.running, g.waiting)
		fmt.Fprintf(w, "vllm:generation_tokens_total %d\nvllm:prompt_tokens_total %d\n", g.genTokens, g.promptToks)
		fmt.Fprintf(w, "mockllm_max_seqs %d\nmockllm_tokens_per_s %g\n", g.maxSeqs, g.tokensPerS)
		for step, n := range g.requests {
			fmt.Fprintf(w, "mockllm_requests_total{step=%q} %d\n", step, n)
		}
	})
	mux.HandleFunc("POST /admin", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxSeqs    int     `json:"max_seqs"`
			TokensPerS float64 `json:"tokens_per_s"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		if req.MaxSeqs > 0 {
			g.maxSeqs = req.MaxSeqs
		}
		if req.TokensPerS > 0 {
			g.tokensPerS = req.TokensPerS
		}
		g.cond.Broadcast()
		g.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	addr := envStr("LISTEN", ":8000")
	log.Printf("mockllm on %s: %d sequences at %d tokens/s", addr, g.maxSeqs, int(g.tokensPerS))
	log.Fatal(http.ListenAndServe(addr, advertise.Wrap(mux, g.maxSeqs)))
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envStr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}
