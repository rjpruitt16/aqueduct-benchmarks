package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	cases := []struct {
		in       string
		min, max time.Duration
	}{
		{"", 0, 0},
		{"5", 5 * time.Second, 5 * time.Second},
		{"0", 0, 0},
		{"-3", 0, 0},
		{"99999999999999", maxRetryAfter, maxRetryAfter},
		{future, 8 * time.Second, 10 * time.Second},
		{past, 0, 0},
		{"soon", 0, 0},
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in); got < c.min || got > c.max {
			t.Errorf("parseRetryAfter(%q) = %s, want between %s and %s", c.in, got, c.min, c.max)
		}
	}
}

func TestJain(t *testing.T) {
	if got := jain([]float64{1, 1, 1}); got != 1 {
		t.Errorf("equal shares: got %v, want 1", got)
	}
	if got := jain([]float64{1, 0, 0, 0}); got != 0.25 {
		t.Errorf("one tenant served: got %v, want 0.25", got)
	}
}

func TestReportCountsOpenDocumentsOnce(t *testing.T) {
	l := newLedger()
	start := time.Now()
	phases := []phase{{name: "steady", length: time.Hour, multiplier: 1}}
	l.offer(&doc{id: "a", tenant: "x", offered: start})
	l.offer(&doc{id: "b", tenant: "x", offered: start})
	l.finish("a", outcomeCompleted, start.Add(time.Second))
	rep := buildReport(l, phases, start, start)
	tt := rep.Tenants["x"]
	if tt.Offered["steady"] != 2 || tt.Outcomes[outcomeCompleted] != 1 || tt.Outcomes["outstanding"] != 1 {
		t.Errorf("want 2 offered, 1 completed, 1 outstanding; got %+v %+v", tt.Offered, tt.Outcomes)
	}
}

func TestEarlyAquiferWebhookIsRetried(t *testing.T) {
	l := newLedger()
	l.offer(&doc{id: "d1", tenant: "x", offered: time.Now()})
	if _, known := l.docForJob("job-1"); known {
		t.Fatal("a job not yet recorded must be unknown, so its webhook is retried")
	}
	l.acceptByAquifer("d1", "job-1", 1, 0, time.Now())
	if id, known := l.docForJob("job-1"); !known || id != "d1" {
		t.Fatalf("got %q %v, want d1 known", id, known)
	}
	l.finish("d1", outcomeCompleted, time.Now())
	if id, known := l.docForJob("job-1"); !known || id != "" {
		t.Fatalf("a finished job is known with no open document; got %q %v", id, known)
	}
}

func TestReportSeparatesAcceptanceRetriesAndBackoff(t *testing.T) {
	l := newLedger()
	start := time.Now()
	phases := []phase{{name: "steady", length: time.Hour}}
	offered := start.Add(time.Second)
	l.offer(&doc{id: "d1", tenant: "small-1", offered: offered})
	l.accept("d1", "run-1", 3, 4*time.Second, offered.Add(5*time.Second))
	l.finish("d1", outcomeCompleted, offered.Add(65*time.Second))
	l.offer(&doc{id: "d2", tenant: "small-1", offered: offered})
	l.accept("d2", "run-2", 1, 0, offered.Add(time.Second))
	l.finish("d2", outcomeCompleted, offered.Add(31*time.Second))

	tt := buildReport(l, phases, start, start.Add(time.Hour)).Tenants["small-1"]
	if tt.Retries != 2 || tt.RetriedDocuments != 1 {
		t.Fatalf("retries %d over %d documents, want 2 over 1", tt.Retries, tt.RetriedDocuments)
	}
	if tt.BackoffSeconds != 4 {
		t.Fatalf("backoff %v s, want 4", tt.BackoffSeconds)
	}
	// Acceptance is offer to first acceptance (1 s and 5 s); completion is
	// offer to completion (30 s and 65 s), never the acceptance.
	if tt.AcceptP99ms["steady"] != 5000 || tt.P99ms["steady"] != 65000 || tt.P50ms["steady"] != 65000 {
		t.Fatalf("accept p99 %v, completion p50/p99 %v/%v", tt.AcceptP99ms["steady"], tt.P50ms["steady"], tt.P99ms["steady"])
	}
}

func TestAcceptanceSeparatesAquiferFromHatchet(t *testing.T) {
	now := time.Now()
	all := []finished{
		{outcome: outcomeCompleted, accepted: now, triggered: now},
		{outcome: outcomeGaveUp, accepted: now},                 // Aquifer's 201, never reached Hatchet
		{outcome: outcomeFailed, accepted: now, triggered: now}, // Hatchet accepted, then failed
		{outcome: outcomeRejected},                              // refused every time
	}
	open := []openDoc{{accepted: now}, {accepted: now, triggered: now}}
	got := acceptance(all, open)
	if got["acked_not_triggered"] != 2 || got["triggered_not_completed"] != 2 || got["never_accepted"] != 1 {
		t.Fatalf("acceptance %v", got)
	}
}

func TestScrapeKeepsMatchingLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "cluster.backend.upstream_rq_retry: 3\ncluster.backend.upstream_rq_total: 40\nserver.uptime: 9\n")
	}))
	defer srv.Close()
	targets := parseScrape("envoy " + srv.URL + " ^cluster\\.backend\\.(upstream_rq_retry|upstream_rq_total);bad;ready " + srv.URL)
	if len(targets) != 2 {
		t.Fatalf("parsed %d targets, want 2", len(targets))
	}
	rec := scrapeOnce(context.Background(), &http.Client{Timeout: time.Second}, targets[0])
	if rec.Status != 200 || len(rec.Lines) != 2 {
		t.Fatalf("got %+v", rec)
	}
	if rec := scrapeOnce(context.Background(), &http.Client{Timeout: time.Second}, targets[1]); rec.Status != 200 || len(rec.Lines) != 0 {
		t.Fatalf("status-only target got %+v", rec)
	}
}
