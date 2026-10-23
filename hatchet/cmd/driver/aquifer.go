package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rjpruitt16/aqueduct-benchmarks/hatchet/internal/pipeline"
)

// aquiferAPI submits documents through Aquifer instead of straight to
// Hatchet (configurations C0 and C). Aquifer stores the job, answers 201
// (with its default 100 ms flush the write reaches disk within 100 ms; see
// AQUIFER_PEBBLE_FLUSH_INTERVAL_MS), and later forwards it to Hatchet's REST trigger at a pace Hatchet can
// take, queueing each tenant separately. Hatchet's response, with the run ID,
// comes back through Aquifer's webhook.
type aquiferAPI struct {
	base       string
	hatchet    *hatchetAPI
	webhookURL string
	maxRetries int
	http       *http.Client
}

// submit hands one document to Aquifer and returns Aquifer's job ID.
func (a *aquiferAPI) submit(ctx context.Context, d pipeline.Document) (string, error) {
	trigger, _ := json.Marshal(map[string]any{
		"workflowName":       pipeline.WorkflowName,
		"input":              d,
		"additionalMetadata": map[string]string{"tenant": d.Tenant, "doc_id": d.DocID},
		"return_only_id":     true,
	})
	body, _ := json.Marshal(map[string]any{
		"user_id":        d.Tenant,
		"idempotent_key": d.DocID,
		"url":            fmt.Sprintf("%s/api/v1/stable/tenants/%s/workflow-runs/trigger", a.hatchet.base, a.hatchet.tenant),
		"method":         http.MethodPost,
		"headers":        map[string]string{"Authorization": "Bearer " + a.hatchet.token, "Content-Type": "application/json"},
		"body":           string(trigger),
		"webhook_url":    a.webhookURL,
		"max_retries":    a.maxRetries,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/jobs", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", &errRetryable{err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		var out struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.JobID == "" {
			return "", fmt.Errorf("aquifer response without a job id: %s", truncate(raw))
		}
		return out.JobID, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return "", &errRetryable{status: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	default:
		return "", fmt.Errorf("aquifer: HTTP %d: %s", resp.StatusCode, truncate(raw))
	}
}

// aquiferWebhook is what Aquifer POSTs when it has finished forwarding a job
// to Hatchet: Hatchet's status code and body, or why it gave up.
type aquiferWebhook struct {
	JobID          string `json:"job_id"`
	Status         string `json:"status"`
	ResponseStatus int    `json:"response_status"`
	Body           string `json:"body"`
	Reason         string `json:"reason"`
}

// runID extracts the Hatchet run ID from a completed forward.
func (w aquiferWebhook) runID() string {
	if w.Status != "completed" || w.ResponseStatus != http.StatusOK {
		return ""
	}
	var out struct {
		Run struct {
			Metadata struct {
				ID string `json:"id"`
			} `json:"metadata"`
		} `json:"run"`
	}
	if err := json.NewDecoder(strings.NewReader(w.Body)).Decode(&out); err != nil {
		return ""
	}
	return out.Run.Metadata.ID
}
