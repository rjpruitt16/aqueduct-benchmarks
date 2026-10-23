package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"time"
)

const interval = 10 * time.Second

// intervalRecord is one line of intervals.jsonl.
type intervalRecord struct {
	At          time.Time               `json:"at"`
	Phase       string                  `json:"phase"`
	Outstanding int                     `json:"outstanding"`
	Retries     int64                   `json:"retries"`
	Tenants     map[string]tenantWindow `json:"tenants"`
}

type tenantWindow struct {
	Finished map[string]int `json:"finished"`
	P50ms    float64        `json:"p50_ms,omitempty"`
	P99ms    float64        `json:"p99_ms,omitempty"`
}

var (
	timelineMu sync.Mutex
	timeline   []intervalRecord // one per interval, kept for the drain-time calculation
)

func (r *run) reportLoop(ctx context.Context, path string) {
	f, err := os.Create(path)
	if err != nil {
		log.Printf("intervals: %v", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	seen := 0
	var lastRetries int64
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		batch, next := r.l.snapshot(seen)
		seen = next
		phaseName, _ := r.phase.Load().(string)
		rec := intervalRecord{At: time.Now().UTC(), Phase: phaseName, Outstanding: r.l.outstanding(), Tenants: map[string]tenantWindow{}}
		retries := r.retries.Load()
		rec.Retries, lastRetries = retries-lastRetries, retries
		lat := map[string][]float64{}
		for _, d := range batch {
			w := rec.Tenants[d.tenant]
			if w.Finished == nil {
				w.Finished = map[string]int{}
			}
			w.Finished[d.outcome]++
			rec.Tenants[d.tenant] = w
			if d.outcome == outcomeCompleted {
				lat[d.tenant] = append(lat[d.tenant], ms(d.completed.Sub(d.offered)))
			}
		}
		for name, xs := range lat {
			w := rec.Tenants[name]
			w.P50ms, w.P99ms = percentile(xs, 0.50), percentile(xs, 0.99)
			rec.Tenants[name] = w
		}
		_ = enc.Encode(rec)
		timelineMu.Lock()
		timeline = append(timeline, rec)
		timelineMu.Unlock()
	}
}

type report struct {
	Started       time.Time               `json:"started"`
	Phases        []phaseWindow           `json:"phases"`
	Tenants       map[string]*tenantTotal `json:"tenants"`
	Totals        map[string]int          `json:"totals"`
	DuplicateDone int                     `json:"duplicate_completions"`
	Outstanding   int                     `json:"outstanding_at_end"`
	// JainOverload is Jain's index over tenants' completed/offered ratios
	// for documents offered during the burst and sustained phases: 1 means
	// every tenant got the same share of its demand served.
	JainOverload float64 `json:"jain_overload"`
	// DrainSeconds is how long after the sustained phase ended the
	// outstanding count fell back to its steady-phase level.
	DrainSeconds float64 `json:"drain_seconds"`
}

type phaseWindow struct {
	Name  string    `json:"name"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type tenantTotal struct {
	Offered  map[string]int     `json:"offered_by_phase"`
	Outcomes map[string]int     `json:"outcomes"`
	P50ms    map[string]float64 `json:"p50_ms_by_phase"`
	P99ms    map[string]float64 `json:"p99_ms_by_phase"`
	// SlowdownP99 is this tenant's p99 for documents offered during the
	// burst and sustained phases, divided by its steady-phase p99. 1 means
	// the overload didn't affect it.
	SlowdownP99 float64 `json:"slowdown_p99"`
	// LongestGapSeconds is the longest stretch during overload in which this
	// tenant completed nothing.
	LongestGapSeconds float64 `json:"longest_gap_s"`
	AvgAttempts       float64 `json:"avg_attempts"`
}

func buildReport(l *ledger, phases []phase, start, end time.Time) report {
	windows := make([]phaseWindow, 0, len(phases))
	t := start
	for _, p := range phases {
		windows = append(windows, phaseWindow{Name: p.name, Start: t, End: t.Add(p.length)})
		t = t.Add(p.length)
	}
	phaseOf := func(at time.Time) string {
		for _, w := range windows {
			if !at.Before(w.Start) && at.Before(w.End) {
				return w.Name
			}
		}
		return "after"
	}
	overload := func(name string) bool { return name == "burst" || name == "sustained" }

	all, open := l.snapshotAll()
	rep := report{Started: start.UTC(), Phases: windows, Tenants: map[string]*tenantTotal{}, Totals: map[string]int{}}
	lat := map[string]map[string][]float64{}
	completions := map[string][]time.Time{}
	attempts := map[string][2]float64{}
	served, demand := map[string]float64{}, map[string]float64{}
	for _, d := range all {
		tt := rep.Tenants[d.tenant]
		if tt == nil {
			tt = &tenantTotal{Offered: map[string]int{}, Outcomes: map[string]int{}, P50ms: map[string]float64{}, P99ms: map[string]float64{}}
			rep.Tenants[d.tenant] = tt
			lat[d.tenant] = map[string][]float64{}
		}
		ph := phaseOf(d.offered)
		tt.Offered[ph]++
		tt.Outcomes[d.outcome]++
		rep.Totals[d.outcome]++
		a := attempts[d.tenant]
		attempts[d.tenant] = [2]float64{a[0] + float64(d.attempts), a[1] + 1}
		if overload(ph) {
			demand[d.tenant]++
		}
		if d.outcome == outcomeCompleted {
			lat[d.tenant][ph] = append(lat[d.tenant][ph], ms(d.completed.Sub(d.offered)))
			if overload(ph) {
				lat[d.tenant]["overload"] = append(lat[d.tenant]["overload"], ms(d.completed.Sub(d.offered)))
				served[d.tenant]++
			}
			completions[d.tenant] = append(completions[d.tenant], d.completed)
		}
	}
	// Documents still open count toward what each tenant offered, but were
	// not served.
	for _, d := range open {
		tt := rep.Tenants[d.tenant]
		if tt == nil {
			tt = &tenantTotal{Offered: map[string]int{}, Outcomes: map[string]int{}, P50ms: map[string]float64{}, P99ms: map[string]float64{}}
			rep.Tenants[d.tenant] = tt
			lat[d.tenant] = map[string][]float64{}
		}
		ph := phaseOf(d.offered)
		tt.Offered[ph]++
		tt.Outcomes["outstanding"]++
		if overload(ph) {
			demand[d.tenant]++
		}
	}
	var ratios []float64
	for name, tt := range rep.Tenants {
		for ph, xs := range lat[name] {
			tt.P50ms[ph], tt.P99ms[ph] = percentile(xs, 0.50), percentile(xs, 0.99)
		}
		if s, o := tt.P99ms["steady"], tt.P99ms["overload"]; s > 0 && o > 0 {
			tt.SlowdownP99 = o / s
		}
		tt.LongestGapSeconds = longestGap(completions[name], windows)
		if a := attempts[name]; a[1] > 0 {
			tt.AvgAttempts = a[0] / a[1]
		}
		if demand[name] > 0 {
			ratios = append(ratios, served[name]/demand[name])
		}
	}
	rep.JainOverload = jain(ratios)
	rep.DuplicateDone = l.duplicates
	rep.Outstanding = len(open)
	rep.DrainSeconds = drainSeconds(windows, len(open))
	return rep
}

// longestGap is the longest interval during the burst and sustained phases
// with no completion for one tenant.
func longestGap(done []time.Time, windows []phaseWindow) float64 {
	var from, to time.Time
	for _, w := range windows {
		if w.Name == "burst" && from.IsZero() {
			from = w.Start
		}
		if w.Name == "sustained" || w.Name == "burst" {
			to = w.End
		}
	}
	if from.IsZero() {
		return 0
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Before(done[j]) })
	last, worst := from, 0.0
	for _, t := range done {
		if t.Before(from) {
			continue
		}
		if t.After(to) {
			break
		}
		worst = math.Max(worst, t.Sub(last).Seconds())
		last = t
	}
	return math.Max(worst, to.Sub(last).Seconds())
}

// drainSeconds is how long after the sustained phase the outstanding count
// took to fall back to 1.5x its steady-phase peak.
//
// If no interval sample caught the drain but the ledger is at the threshold
// now, the drain happened after the last sample, and the time since the
// sustained phase is returned as an upper bound.
func drainSeconds(windows []phaseWindow, outstandingNow int) float64 {
	timelineMu.Lock()
	defer timelineMu.Unlock()
	var sustainedEnd time.Time
	for _, w := range windows {
		if w.Name == "sustained" {
			sustainedEnd = w.End
		}
	}
	steadyPeak := 0
	for _, rec := range timeline {
		if rec.Phase == "steady" && rec.Outstanding > steadyPeak {
			steadyPeak = rec.Outstanding
		}
	}
	if sustainedEnd.IsZero() {
		return 0
	}
	limit := int(math.Max(1.5*float64(steadyPeak), 10))
	for _, rec := range timeline {
		if rec.At.After(sustainedEnd) && rec.Outstanding <= limit {
			return rec.At.Sub(sustainedEnd).Seconds()
		}
	}
	if outstandingNow <= limit {
		return time.Since(sustainedEnd).Seconds()
	}
	return -1 // never drained within the run
}

// jain is Jain's fairness index: (sum x)^2 / (n * sum x^2).
func jain(xs []float64) float64 {
	var sum, sq float64
	for _, x := range xs {
		sum += x
		sq += x * x
	}
	if sq == 0 {
		return 0
	}
	return sum * sum / (float64(len(xs)) * sq)
}

func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return math.Round(s[int(math.Min(float64(len(s)-1), q*float64(len(s))))]*10) / 10
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
