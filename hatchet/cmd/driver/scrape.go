package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// scrapeTarget is one endpoint the driver samples during the run: a gateway's
// readiness or counters, or Aquifer's metrics. Sampling from here, not once
// at the end, keeps counters that reset when a process restarts, and times
// a gateway's recovery to within one interval.
type scrapeTarget struct {
	name  string
	url   string
	lines *regexp.Regexp // keep only matching lines; nil keeps none (status only)
}

// parseScrape reads SCRAPE: entries separated by ";", each "name url [regexp]"
// separated by spaces (a URL or a pattern may contain "|", never a space).
func parseScrape(spec string) []scrapeTarget {
	var out []scrapeTarget
	for _, part := range strings.Split(spec, ";") {
		f := strings.SplitN(strings.TrimSpace(part), " ", 3)
		if len(f) < 2 || f[0] == "" || f[1] == "" {
			continue
		}
		t := scrapeTarget{name: f[0], url: f[1]}
		if len(f) == 3 && f[2] != "" {
			re, err := regexp.Compile(f[2])
			if err != nil {
				log.Printf("SCRAPE %s: %v", f[0], err)
				continue
			}
			t.lines = re
		}
		out = append(out, t)
	}
	return out
}

type scrapeRecord struct {
	At     time.Time `json:"at"`
	Name   string    `json:"name"`
	Status int       `json:"status"` // 0: no answer
	Error  string    `json:"error,omitempty"`
	Lines  []string  `json:"lines,omitempty"`
}

// scrapeLoop samples every target every interval into path, one JSON line per
// sample, until ctx ends.
func scrapeLoop(ctx context.Context, targets []scrapeTarget, every time.Duration, path string) {
	if len(targets) == 0 {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		log.Printf("scrape: %v", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	client := &http.Client{Timeout: every}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		for _, t := range targets {
			enc.Encode(scrapeOnce(ctx, client, t))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func scrapeOnce(ctx context.Context, client *http.Client, t scrapeTarget) scrapeRecord {
	rec := scrapeRecord{At: time.Now().UTC(), Name: t.name}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
	resp, err := client.Do(req)
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode
	if t.lines == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return rec
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 8<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if line := sc.Text(); t.lines.MatchString(line) {
			rec.Lines = append(rec.Lines, line)
		}
	}
	return rec
}
