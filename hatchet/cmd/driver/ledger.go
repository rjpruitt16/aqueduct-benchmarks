package main

import (
	"sync"
	"time"
)

// Outcomes a document can end in. A document starts "offered", becomes
// "accepted" once Hatchet (or Aquifer) has durably stored it, and ends in one
// of the terminal outcomes.
const (
	outcomeCompleted    = "completed"
	outcomeFailed       = "failed"        // ran and failed
	outcomeNeverStarted = "never_started" // timed out in the queue before running (#4287)
	outcomeRejected     = "rejected"      // never accepted: retries exhausted
	// outcomeGaveUp: accepted by Aquifer, which then ran out of retries
	// forwarding it to Hatchet. Counted separately because the producer was
	// told the document was accepted.
	outcomeGaveUp = "gave_up"
)

type doc struct {
	id        string
	tenant    string
	offered   time.Time
	accepted  time.Time
	completed time.Time
	runID     string
	jobID     string    // Aquifer's job ID (C0, C)
	triggered time.Time // when Hatchet accepted it; equals accepted without Aquifer
	attempts  int
	outcome   string
	// lastPolled spaces out status polls for this document.
	lastPolled time.Time
}

// ledger tracks every offered document until it reaches an outcome. Open
// documents live in a map; terminal ones are kept as small records for the
// final report, so memory grows with the run's length (about 64 bytes per
// document, roughly 150MB for a 75-minute run at 500 documents/s).
type ledger struct {
	mu         sync.Mutex
	open       map[string]*doc
	byRun      map[string]string // run ID -> doc ID, for callbacks
	byJob      map[string]string // Aquifer job ID -> doc ID, for Aquifer's webhook
	seenJobs   map[string]bool   // every Aquifer job ID accepted, to tell early webhooks from late ones
	duplicates int               // completions reported more than once
	finished   []finished        // terminal documents, for the report
}

type finished struct {
	tenant    string
	offered   time.Time
	triggered time.Time // zero if it never reached Hatchet
	completed time.Time
	outcome   string
	attempts  int
}

func newLedger() *ledger {
	return &ledger{open: map[string]*doc{}, byRun: map[string]string{}, byJob: map[string]string{}, seenJobs: map[string]bool{}}
}

func (l *ledger) offer(d *doc) {
	l.mu.Lock()
	l.open[d.id] = d
	l.mu.Unlock()
}

func (l *ledger) accept(id, runID string, attempts int, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.open[id]
	if !ok {
		return
	}
	d.accepted, d.runID, d.attempts = at, runID, attempts
	if runID != "" {
		d.triggered = at
		l.byRun[runID] = id
	}
}

// acceptByAquifer records Aquifer's durable acceptance; the Hatchet run ID
// arrives later through attachRun.
func (l *ledger) acceptByAquifer(id, jobID string, attempts int, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.open[id]
	if !ok {
		return
	}
	d.accepted, d.jobID, d.attempts = at, jobID, attempts
	l.byJob[jobID] = id
	l.seenJobs[jobID] = true
}

// attachRun records that Aquifer's forward reached Hatchet. It returns the
// document ID, or "" if the job is unknown or already finished.
func (l *ledger) attachRun(jobID, runID string, at time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	id, ok := l.byJob[jobID]
	if !ok {
		return ""
	}
	delete(l.byJob, jobID)
	d, ok := l.open[id]
	if !ok {
		return ""
	}
	d.runID, d.triggered = runID, at
	l.byRun[runID] = id
	return id
}

// docForJob returns the document behind an Aquifer job, and whether the job
// is known at all. A webhook can arrive before submit has recorded its job
// ID; known is false then, and the webhook should be retried.
func (l *ledger) docForJob(jobID string) (id string, known bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byJob[jobID], l.seenJobs[jobID]
}

// finish records a terminal outcome. A document that already finished counts
// as a duplicate completion instead.
func (l *ledger) finish(id, outcome string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.open[id]
	if !ok {
		if outcome == outcomeCompleted {
			l.duplicates++
		}
		return
	}
	delete(l.open, id)
	delete(l.byRun, d.runID)
	delete(l.byJob, d.jobID)
	l.finished = append(l.finished, finished{tenant: d.tenant, offered: d.offered, triggered: d.triggered, completed: at, outcome: outcome, attempts: d.attempts})
}

// due returns accepted, unfinished documents not polled within every.
func (l *ledger) due(every time.Duration, now time.Time, limit int) []*doc {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*doc
	for _, d := range l.open {
		if d.runID == "" || now.Sub(d.lastPolled) < every {
			continue
		}
		d.lastPolled = now
		out = append(out, &doc{id: d.id, runID: d.runID})
		if len(out) == limit {
			break
		}
	}
	return out
}

func (l *ledger) outstanding() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.open)
}

// openDoc is an unfinished document, for the final report.
type openDoc struct {
	tenant    string
	offered   time.Time
	triggered time.Time
}

// snapshotAll returns every finished and every open document under one lock,
// so a document finishing while the report is built is counted exactly once.
func (l *ledger) snapshotAll() ([]finished, []openDoc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	done := append([]finished(nil), l.finished...)
	open := make([]openDoc, 0, len(l.open))
	for _, d := range l.open {
		open = append(open, openDoc{tenant: d.tenant, offered: d.offered, triggered: d.triggered})
	}
	return done, open
}

// snapshot copies terminal documents finished since index from.
func (l *ledger) snapshot(from int) ([]finished, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]finished(nil), l.finished[from:]...)
	return out, len(l.finished)
}
