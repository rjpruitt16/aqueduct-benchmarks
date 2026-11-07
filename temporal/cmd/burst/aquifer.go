package main

// runAquifer offers the same load through Aquifer: each start is a POST /jobs
// aimed at Temporal's HTTP API (start workflow), retried by Aquifer until it
// succeeds. A caller counts a start as accepted on Aquifer's 201 and as done
// when Aquifer's completion webhook reports Temporal's answer. Refusals at
// intake are retried by the caller with backoff for up to INTAKE_GIVE_UP.
//
// Environment: AQUIFER_URL, TEMPORAL_HTTP (default http://localhost:7243),
// LISTEN (webhook listener, default :9099), PUBLIC_URL (default
// http://localhost:9099), TENANTS (default 20), DRAIN_MAX (default 20m),
// INTAKE_GIVE_UP (default 60s), plus RATE, DURATION, CALLERS, OUT.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func runAquifer() {
	base := strings.TrimRight(os.Getenv("AQUIFER_URL"), "/")
	temporalHTTP := strings.TrimRight(env("TEMPORAL_HTTP", "http://localhost:7243"), "/")
	rate, duration, callers := envInt("RATE", 600), envDuration("DURATION", 3*time.Minute), envInt("CALLERS", 2000)
	tenants := envInt("TENANTS", 20)
	giveUp := envDuration("INTAKE_GIVE_UP", time.Minute)
	webhook := strings.TrimRight(env("PUBLIC_URL", "http://localhost:9099"), "/") + "/hook"

	var mu sync.Mutex
	offeredAt := map[string]time.Time{} // job_id -> offered
	var latencies []float64
	failed := map[string]int{}
	var done, intakeRefused, intakeAttempts atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", func(w http.ResponseWriter, r *http.Request) {
		var h struct {
			JobID          string `json:"job_id"`
			Status         string `json:"status"`
			ResponseStatus int    `json:"response_status"`
			Reason         string `json:"reason"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&h); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		at, ok := offeredAt[h.JobID]
		if !ok {
			mu.Unlock()
			// Delivered before the submitter recorded the job: ask Aquifer to
			// redeliver.
			http.Error(w, "not recorded yet", http.StatusServiceUnavailable)
			return
		}
		delete(offeredAt, h.JobID)
		if h.Status == "completed" && h.ResponseStatus/100 == 2 {
			latencies = append(latencies, time.Since(at).Seconds())
		} else {
			failed[fmt.Sprintf("%s %d %s", h.Status, h.ResponseStatus, h.Reason)]++
		}
		mu.Unlock()
		done.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Addr: env("LISTEN", ":9099"), Handler: mux}
	go srv.ListenAndServe()
	defer srv.Close()

	httpc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 1024}}
	offers := make(chan time.Time, rate*int(duration.Seconds())+1)
	// Unique per run: a reused ID is (correctly) a duplicate to Aquifer and
	// to Temporal, and would never report back.
	runID := strconv.FormatInt(time.Now().Unix(), 36)
	var seq atomic.Int64
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for offered := range offers {
				n := seq.Add(1)
				id := fmt.Sprintf("aq-%s-%d", runID, n)
				startBody, _ := json.Marshal(map[string]any{
					"workflowType": map[string]string{"name": "Noop"},
					"taskQueue":    map[string]string{"name": "bench"},
				})
				job, _ := json.Marshal(map[string]any{
					"user_id":        fmt.Sprintf("tenant-%d", n%int64(tenants)),
					"idempotent_key": id,
					"url":            fmt.Sprintf("%s/api/v1/namespaces/default/workflows/%s", temporalHTTP, id),
					"method":         "POST",
					"headers":        map[string]string{"Content-Type": "application/json"},
					"body":           string(startBody),
					"webhook_url":    webhook,
					"max_retries":    -1,
				})
				backoff := 100 * time.Millisecond
				for {
					intakeAttempts.Add(1)
					resp, err := httpc.Post(base+"/jobs", "application/json", bytes.NewReader(job))
					if err == nil {
						raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
						resp.Body.Close()
						if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
							var out struct {
								JobID string `json:"job_id"`
							}
							json.Unmarshal(raw, &out)
							mu.Lock()
							offeredAt[out.JobID] = offered
							mu.Unlock()
							accepted.Add(1)
							break
						}
						intakeRefused.Add(1)
					}
					if time.Since(offered) > giveUp {
						mu.Lock()
						failed["refused at intake"]++
						mu.Unlock()
						done.Add(1)
						break
					}
					time.Sleep(backoff/2 + time.Duration(rand.Int64N(int64(backoff/2))))
					backoff = min(backoff*2, 5*time.Second)
				}
			}
		}()
	}

	start := time.Now()
	tick := time.NewTicker(10 * time.Millisecond)
	owed, offered := 0.0, 0
	lastLog := time.Now()
	for time.Since(start) < duration {
		<-tick.C
		owed += float64(rate) / 100
		for ; owed >= 1; owed-- {
			offers <- time.Now()
			offered++
		}
		if time.Since(lastLog) > 15*time.Second {
			lastLog = time.Now()
			log.Printf("t=%.0fs offered %d accepted %d done %d intake refusals %d", time.Since(start).Seconds(), offered, accepted.Load(), done.Load(), intakeRefused.Load())
		}
	}
	tick.Stop()
	close(offers)
	wg.Wait()
	deadline := time.Now().Add(envDuration("DRAIN_MAX", 20*time.Minute))
	for done.Load() < int64(offered) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		log.Printf("draining: t=%.0fs done %d of %d", time.Since(start).Seconds(), done.Load(), offered)
	}

	mu.Lock()
	defer mu.Unlock()
	sort.Float64s(latencies)
	pct := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[min(len(latencies)-1, int(p*float64(len(latencies))))]
	}
	attempts := targetAttempts(base, temporalHTTP)
	summary := map[string]any{
		"via": "aquifer", "rate": rate, "duration_s": duration.Seconds(), "callers": callers, "tenants": tenants,
		"offered": offered, "accepted": accepted.Load(), "started": len(latencies), "failed": failed,
		"unfinished":      offered - int(done.Load()),
		"intake_attempts": intakeAttempts.Load(), "intake_refused": intakeRefused.Load(),
		"temporal_attempts_by_status": attempts,
		"latency_p50_s":               pct(0.5), "latency_p99_s": pct(0.99),
		"wall_s": time.Since(start).Seconds(),
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	os.WriteFile(env("OUT", "burst-aquifer.json"), b, 0o644)
	fmt.Println(string(b))
}

// targetAttempts reads Aquifer's dispatch attempts at Temporal, by status
// class, from GET /metrics.
func targetAttempts(base, temporalHTTP string) map[string]float64 {
	out := map[string]float64{}
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	host := strings.TrimPrefix(strings.TrimPrefix(temporalHTTP, "http://"), "https://")
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "aquifer_dispatch_attempts_total{") || !strings.Contains(line, host) {
			continue
		}
		i := strings.Index(line, "status_class=\"")
		j := strings.LastIndex(line, " ")
		if i < 0 || j < 0 {
			continue
		}
		class := line[i+len("status_class=\""):]
		class = class[:strings.Index(class, "\"")]
		var v float64
		fmt.Sscanf(line[j+1:], "%g", &v)
		out[class] += v
	}
	return out
}
