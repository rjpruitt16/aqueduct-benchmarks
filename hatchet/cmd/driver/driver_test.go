package main

import (
	"net/http"
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
