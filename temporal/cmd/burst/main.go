// Command burst offers workflow starts to Temporal at a fixed rate (open
// loop, like many independent callers) through the Go SDK with its default
// retries, and reports what callers saw: starts that failed, start latency
// from the moment each was offered, and how many StartWorkflowExecution
// attempts and ResourceExhausted refusals it took.
//
// With AQUIFER_URL set, starts go through Aquifer instead (see aquifer.go).
//
// Environment: TEMPORAL_ADDRESS (default localhost:7233), RATE (starts/s,
// default 600), DURATION (default 3m), CALLERS (concurrent callers, default
// 2000), OUT (summary JSON, default burst.json).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type result struct {
	latency time.Duration
	err     string
}

func main() {
	if os.Getenv("AQUIFER_URL") != "" {
		runAquifer()
		return
	}
	rate := envInt("RATE", 600)
	duration := envDuration("DURATION", 3*time.Minute)
	callers := envInt("CALLERS", 2000)

	var attempts, exhausted atomic.Int64
	count := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		err := invoker(ctx, method, req, reply, cc, opts...)
		if strings.HasSuffix(method, "/StartWorkflowExecution") {
			attempts.Add(1)
			if status.Code(err) == codes.ResourceExhausted {
				exhausted.Add(1)
			}
		}
		return err
	}
	c, err := client.Dial(client.Options{
		HostPort: env("TEMPORAL_ADDRESS", "localhost:7233"),
		ConnectionOptions: client.ConnectionOptions{
			DialOptions: []grpc.DialOption{grpc.WithChainUnaryInterceptor(count)},
		},
	})
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()

	offers := make(chan time.Time, rate*int(duration.Seconds())+1)
	results := make(chan result, cap(offers))
	runID := strconv.FormatInt(time.Now().Unix(), 36) // unique IDs per run
	var seq atomic.Int64
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for offered := range offers {
				id := fmt.Sprintf("burst-%s-%d", runID, seq.Add(1))
				_, err := c.ExecuteWorkflow(context.Background(),
					client.StartWorkflowOptions{ID: id, TaskQueue: "bench"}, "Noop")
				r := result{latency: time.Since(offered)}
				if err != nil {
					r.err = status.Code(err).String()
					if r.err == "OK" || r.err == "Unknown" {
						r.err = err.Error()
					}
				}
				results <- r
			}
		}()
	}

	start := time.Now()
	tick := time.NewTicker(10 * time.Millisecond)
	owed := 0.0
	offered := 0
	for time.Since(start) < duration {
		<-tick.C
		owed += float64(rate) / 100
		for ; owed >= 1; owed-- {
			offers <- time.Now()
			offered++
		}
		if offered%(rate*10) == 0 {
			log.Printf("t=%.0fs offered %d attempts %d refused %d", time.Since(start).Seconds(), offered, attempts.Load(), exhausted.Load())
		}
	}
	tick.Stop()
	close(offers)
	wg.Wait()
	close(results)

	var lat []float64
	failed := map[string]int{}
	var slowestOK time.Duration
	for r := range results {
		if r.err != "" {
			failed[r.err]++
			continue
		}
		lat = append(lat, r.latency.Seconds())
		slowestOK = max(slowestOK, r.latency)
	}
	sort.Float64s(lat)
	pct := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(len(lat)-1, int(p*float64(len(lat))))]
	}
	summary := map[string]any{
		"rate": rate, "duration_s": duration.Seconds(), "callers": callers,
		"offered": offered, "started": len(lat), "failed": failed,
		"start_attempts": attempts.Load(), "resource_exhausted": exhausted.Load(),
		"attempts_per_offer": float64(attempts.Load()) / float64(offered),
		"latency_p50_s":      pct(0.5), "latency_p99_s": pct(0.99), "latency_max_ok_s": slowestOK.Seconds(),
		"wall_s": time.Since(start).Seconds(),
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	os.WriteFile(env("OUT", "burst.json"), b, 0o644)
	fmt.Println(string(b))
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
