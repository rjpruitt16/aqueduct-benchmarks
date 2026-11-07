// Command backlog measures whether a deep Hatchet backlog makes each event
// more expensive to process (EVENTS.md, step 0).
//
//	backlog push   pushes N events for S sources through Hatchet's bulk event
//	               API while no worker is running, so they all queue.
//	backlog sustain  pushes RATE events/s for DURATION while a worker runs,
//	               and samples like watch, with remaining = pushed - completed:
//	               a backlog that builds while the worker is held back (by a
//	               rate limit), as in a sale against a rate-limited downstream.
//	               Stops early if the disk has less than MIN_FREE_GB free.
//	backlog watch  while a worker drains them, samples every INTERVAL: events
//	               completed (the worker's /count), and the CPU time Postgres
//	               and the engine used (their cgroup cpu.stat), and writes one
//	               JSON line per sample to OUT. Stops when the count reaches N
//	               or stops moving for STALL.
//
// Environment: the Hatchet client's (HATCHET_CLIENT_TOKEN, ...), N, SOURCES, BATCH (default 500), PUSHERS (default 8),
// COUNT_URL (default http://localhost:8091/count), CONTAINERS (compose
// services to sample, default postgres,engine), INTERVAL (default 5s), STALL
// (default 60s), OUT (default backlog.jsonl).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/rjpruitt16/aqueduct-benchmarks/hatchet/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: backlog push|watch")
	}
	switch os.Args[1] {
	case "push":
		push()
	case "watch":
		watch()
	case "sustain":
		sustain()
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func push() {
	// Through the SDK's gRPC client, as producers do: the REST bulk endpoint
	// is deprecated on v0.110.8. It reads HATCHET_CLIENT_* like the worker.
	client, err := hatchet.NewClient()
	if err != nil {
		log.Fatalf("hatchet client: %v", err)
	}
	n, sources := envInt("N", 10000), envInt("SOURCES", 200)
	batch, pushers := envInt("BATCH", 500), envInt("PUSHERS", 8)

	jobs := make(chan int)
	var sent atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for range pushers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for from := range jobs {
				var events []hatchet.EventWithAdditionalMetadata
				for i := from; i < min(from+batch, n); i++ {
					ev := pipeline.Event{ID: fmt.Sprintf("e%d", i), Source: fmt.Sprintf("s%d", i%sources)}
					events = append(events, hatchet.EventWithAdditionalMetadata{Key: pipeline.EventKey, Event: ev})
				}
				for attempt := 1; ; attempt++ {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					err := client.Events().BulkPush(ctx, events)
					cancel()
					if err == nil {
						break
					}
					if attempt == 10 {
						log.Fatalf("batch at %d: %v", from, err)
					}
					log.Printf("batch at %d, attempt %d: %v", from, attempt, err)
					time.Sleep(time.Duration(attempt) * time.Second)
				}
				if done := sent.Add(int64(min(batch, n-from))); done%50000 < int64(batch) {
					log.Printf("pushed %d of %d", done, n)
				}
			}
		}()
	}
	for from := 0; from < n; from += batch {
		jobs <- from
	}
	close(jobs)
	wg.Wait()
	log.Printf("pushed %d events for %d sources in %s", n, sources, time.Since(start).Round(time.Second))
}

type sample struct {
	T         float64            `json:"t"`         // seconds since watch started
	Completed int64              `json:"completed"` // events completed so far
	Remaining int64              `json:"remaining"`
	CPU       map[string]float64 `json:"cpu_s"` // cumulative CPU seconds per container since start
}

func watch() {
	n := int64(envInt("N", 10000))
	countURL := env("COUNT_URL", "http://localhost:8091/count")
	interval, stall := envDuration("INTERVAL", 5*time.Second), envDuration("STALL", time.Minute)
	ids := containerIDs()
	f, err := os.Create(env("OUT", "backlog.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	base := map[string]float64{}
	for c, id := range ids {
		base[c] = cpuSeconds(id)
	}
	start := time.Now()
	last, lastMoved := int64(-1), time.Now()
	for {
		done := count(countURL)
		s := sample{T: time.Since(start).Seconds(), Completed: done, Remaining: n - done, CPU: map[string]float64{}}
		for c, id := range ids {
			s.CPU[c] = cpuSeconds(id) - base[c]
		}
		if err := enc.Encode(s); err != nil {
			log.Fatal(err)
		}
		log.Printf("t=%.0fs completed %d remaining %d cpu %v", s.T, done, s.Remaining, s.CPU)
		if done != last {
			last, lastMoved = done, time.Now()
		}
		if done >= n || time.Since(lastMoved) > stall {
			return
		}
		time.Sleep(interval)
	}
}

func sustain() {
	client, err := hatchet.NewClient()
	if err != nil {
		log.Fatalf("hatchet client: %v", err)
	}
	rate := envInt("RATE", 400)
	sources := envInt("SOURCES", 200)
	duration := envDuration("DURATION", 20*time.Minute)
	interval := envDuration("INTERVAL", 10*time.Second)
	minFree := float64(envInt("MIN_FREE_GB", 10))
	countURL := env("COUNT_URL", "http://localhost:8091/count")
	ids := containerIDs()
	f, err := os.Create(env("OUT", "sustain.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	// One batch every 100 ms keeps the offered rate steady.
	var pushed atomic.Int64
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		per := rate / 10
		for seq := 0; ; {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			events := make([]hatchet.EventWithAdditionalMetadata, 0, per)
			for range per {
				ev := pipeline.Event{ID: fmt.Sprintf("e%d", seq), Source: fmt.Sprintf("s%d", seq%sources)}
				events = append(events, hatchet.EventWithAdditionalMetadata{Key: pipeline.EventKey, Event: ev})
				seq++
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := client.Events().BulkPush(ctx, events); err != nil {
				log.Printf("push: %v", err)
			} else {
				pushed.Add(int64(len(events)))
			}
			cancel()
		}
	}()

	base := map[string]float64{}
	for c, id := range ids {
		base[c] = cpuSeconds(id)
	}
	start := time.Now()
	for time.Since(start) < duration {
		time.Sleep(interval)
		done := count(countURL)
		s := sample{T: time.Since(start).Seconds(), Completed: done, Remaining: pushed.Load() - done, CPU: map[string]float64{}}
		for c, id := range ids {
			s.CPU[c] = cpuSeconds(id) - base[c]
		}
		if err := enc.Encode(s); err != nil {
			log.Fatal(err)
		}
		free := freeGB()
		log.Printf("t=%.0fs pushed %d completed %d backlog %d cpu %v free %.0fGB", s.T, pushed.Load(), done, s.Remaining, s.CPU, free)
		if free < minFree {
			log.Printf("stopping: only %.0f GB free", free)
			break
		}
	}
	close(stop)
}

// freeGB is the free space on the root volume, or a large number if it
// can't be read.
func freeGB() float64 {
	out, err := exec.Command("df", "-k", "/").Output()
	if err != nil {
		return 1e9
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 1e9
	}
	kb, err := strconv.ParseFloat(fields[3], 64)
	if err != nil {
		return 1e9
	}
	return kb / (1 << 20)
}

func containerIDs() map[string]string {
	ids := map[string]string{}
	for _, c := range strings.Split(env("CONTAINERS", "postgres,engine"), ",") {
		out, err := exec.Command("docker", "compose", "-f", env("COMPOSE_FILE", "deploy/local/compose.yml"), "ps", "-q", c).Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			log.Fatalf("container for %s: %v", c, err)
		}
		ids[c] = strings.TrimSpace(string(out))
	}
	return ids
}

func count(url string) int64 {
	resp, err := http.Get(url)
	if err != nil {
		log.Printf("count: %v", err)
		return 0
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	v, _ := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return v
}

// cpuSeconds is the container's cumulative CPU time from its cgroup.
func cpuSeconds(id string) float64 {
	out, err := exec.Command("docker", "exec", id, "cat", "/sys/fs/cgroup/cpu.stat").Output()
	if err != nil {
		log.Printf("cpu.stat for %s: %v", id, err)
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
			us, _ := strconv.ParseFloat(v, 64)
			return us / 1e6
		}
	}
	return 0
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

func envDuration(name string, fallback time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return fallback
}
