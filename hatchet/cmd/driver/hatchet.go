package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rjpruitt16/aqueduct-benchmarks/hatchet/internal/pipeline"
)

// hatchetAPI is the slice of Hatchet's REST API the producers use: trigger a
// run, and poll its status the way an application would.
type hatchetAPI struct {
	base   string
	token  string
	tenant string
	http   *http.Client
}

// errRetryable marks a submission the producer should retry; retryAfter is
// the server's hint, if any.
type errRetryable struct {
	status     int
	retryAfter time.Duration
	err        error
}

func (e *errRetryable) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

// trigger starts a document's workflow run and returns its run ID.
func (h *hatchetAPI) trigger(ctx context.Context, d pipeline.Document) (string, error) {
	onlyID := true
	body, _ := json.Marshal(map[string]any{
		"workflowName":       pipeline.WorkflowName,
		"input":              d,
		"additionalMetadata": map[string]string{"tenant": d.Tenant, "doc_id": d.DocID},
		"return_only_id":     onlyID,
	})
	url := fmt.Sprintf("%s/api/v1/stable/tenants/%s/workflow-runs/trigger", h.base, h.tenant)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.http.Do(req)
	if err != nil {
		return "", &errRetryable{err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK:
		var out struct {
			Run struct {
				Metadata struct {
					ID string `json:"id"`
				} `json:"metadata"`
			} `json:"run"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Run.Metadata.ID == "" {
			return "", fmt.Errorf("trigger response without a run id: %s", truncate(raw))
		}
		return out.Run.Metadata.ID, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return "", &errRetryable{status: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	default:
		// Stock Hatchet answers an engine ResourceExhausted with 400. Treat
		// it as backpressure, the way a careful application would once it
		// learned what that 400 means.
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(raw)), "resource exhausted") {
			return "", &errRetryable{status: resp.StatusCode}
		}
		return "", fmt.Errorf("trigger: HTTP %d: %s", resp.StatusCode, truncate(raw))
	}
}

// status returns a run's status (QUEUED, RUNNING, COMPLETED, FAILED,
// CANCELLED).
func (h *hatchetAPI) status(ctx context.Context, runID string) (string, error) {
	url := fmt.Sprintf("%s/api/v1/stable/workflow-runs/%s/status", h.base, runID)
	raw, err := h.get(ctx, url)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("status response: %s", truncate(raw))
	}
	return s, nil
}

// neverStarted reports whether a failed run failed before any of its tasks
// started: a scheduling timeout (#4287) rather than a task error.
func (h *hatchetAPI) neverStarted(ctx context.Context, runID string) (bool, error) {
	url := fmt.Sprintf("%s/api/v1/stable/workflow-runs/%s", h.base, runID)
	raw, err := h.get(ctx, url)
	if err != nil {
		return false, err
	}
	var details struct {
		Tasks []struct {
			StartedAt *string `json:"startedAt"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		return false, err
	}
	for _, t := range details.Tasks {
		if t.StartedAt != nil && *t.StartedAt != "" {
			return false, nil
		}
	}
	return true, nil
}

func (h *hatchetAPI) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return raw, nil
}

// tenantFromToken reads the Hatchet tenant ID from the token's sub claim.
// The token is our own, so this only decodes it; Hatchet verifies it.
func tenantFromToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", errors.New("token has no sub claim")
	}
	return claims.Sub, nil
}

// maxRetryAfter caps how long one Retry-After hint can hold a submitter.
const maxRetryAfter = 60 * time.Second

// parseRetryAfter reads Retry-After as seconds or an HTTP date, capped at
// maxRetryAfter. Past, unparsable or non-positive values return 0, so the
// producer's own backoff applies.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if s, err := strconv.ParseInt(v, 10, 64); err == nil {
		if s <= 0 {
			return 0
		}
		if s > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		d = time.Duration(s) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	if d <= 0 {
		return 0
	}
	return min(d, maxRetryAfter)
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
