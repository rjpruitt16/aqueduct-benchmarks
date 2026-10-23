// Command worker runs the benchmark's document pipeline on Hatchet: four
// tasks per document (parse, extract, embed, index), each a fixed-duration
// simulation, so Postgres sees the pipeline's write pattern without real PDF
// work. The index step reports completion to the driver's callback URL.
//
// Environment:
//
//	CONFIG            A (stock) or B (tuned); C0 and C use B's settings.
//	STEP_MS           duration of each step (default 50).
//	SLOTS             worker slots (default 200).
//	TENANT_MAX_RUNS   B only: concurrent documents per tenant (default 50).
//	SCHEDULE_TIMEOUT  B only: how long a task may wait to start (default 1h).
//	CALLBACK_URL      where the index step POSTs completions.
//
// Plus the Hatchet client's own variables (HATCHET_CLIENT_TOKEN, ...).
package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/hatchet-dev/hatchet/pkg/cmdutils"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/rjpruitt16/aqueduct-benchmarks/hatchet/internal/pipeline"
)

type stepOutput struct {
	Step string `json:"step"`
}

func main() {
	config := env("CONFIG", "A")
	step := time.Duration(envInt("STEP_MS", 50)) * time.Millisecond
	callback := os.Getenv("CALLBACK_URL")
	if callback != "" {
		if u, err := url.Parse(callback); err != nil || u.Scheme == "" || u.Host == "" {
			log.Fatalf("CALLBACK_URL %q is not an absolute URL", callback)
		}
	}

	client, err := hatchet.NewClient()
	if err != nil {
		log.Fatalf("hatchet client: %v", err)
	}

	var wfOpts []hatchet.WorkflowOption
	var taskOpts []hatchet.TaskOption
	if config != "A" {
		maxRuns := int32(envInt("TENANT_MAX_RUNS", 50))
		strategy := hatchet.GroupRoundRobin
		wfOpts = append(wfOpts, hatchet.WithWorkflowConcurrency(hatchet.Concurrency{
			Expression:    "input.tenant",
			MaxRuns:       &maxRuns,
			LimitStrategy: &strategy,
		}))
		timeout, err := time.ParseDuration(env("SCHEDULE_TIMEOUT", "1h"))
		if err != nil {
			log.Fatalf("SCHEDULE_TIMEOUT: %v", err)
		}
		taskOpts = append(taskOpts, hatchet.WithScheduleTimeout(timeout))
	}

	wf := client.NewWorkflow(pipeline.WorkflowName, wfOpts...)
	sim := func(name string) func(hatchet.Context, pipeline.Document) (stepOutput, error) {
		return func(ctx hatchet.Context, doc pipeline.Document) (stepOutput, error) {
			time.Sleep(step)
			return stepOutput{Step: name}, nil
		}
	}
	parse := wf.NewTask("parse", sim("parse"), taskOpts...)
	extract := wf.NewTask("extract", sim("extract"), append(taskOpts, hatchet.WithParents(parse))...)
	embed := wf.NewTask("embed", sim("embed"), append(taskOpts, hatchet.WithParents(extract))...)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	wf.NewTask("index", func(ctx hatchet.Context, doc pipeline.Document) (stepOutput, error) {
		time.Sleep(step)
		if callback != "" {
			body, _ := json.Marshal(pipeline.Completion{DocID: doc.DocID, RunID: ctx.WorkflowRunId()})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, bytes.NewReader(body))
			if err != nil {
				log.Printf("callback for %s: %v", doc.DocID, err)
				return stepOutput{Step: "index"}, nil
			}
			req.Header.Set("Content-Type", "application/json")
			if resp, err := httpClient.Do(req); err != nil {
				// The driver also polls run status, so a missed callback is
				// recovered there rather than failing the document.
				log.Printf("callback for %s: %v", doc.DocID, err)
			} else {
				resp.Body.Close()
			}
		}
		return stepOutput{Step: "index"}, nil
	}, append(taskOpts, hatchet.WithParents(embed))...)

	worker, err := client.NewWorker("pipeline-worker",
		hatchet.WithWorkflows(wf), hatchet.WithSlots(envInt("SLOTS", 200)))
	if err != nil {
		log.Fatalf("worker: %v", err)
	}
	ctx, cancel := cmdutils.NewInterruptContext()
	defer cancel()
	log.Printf("worker: config %s, %s per step", config, step)
	if err := worker.StartBlocking(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("worker stopped: %v", err)
	}
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
