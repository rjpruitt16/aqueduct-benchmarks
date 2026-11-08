// Command worker runs the "Noop" workflows the burst starts, on task queue
// "bench", so the worker's own calls (task completions, activity results)
// compete with starts for the namespace limit, as they do in production.
// Each workflow runs ACTIVITIES no-op activities (default 1).
//
// It logs, every 10 s, how many workflows completed and how many calls it
// made to the frontend, and how many of those were refused.
//
// Environment: TEMPORAL_ADDRESS (default localhost:7233), ACTIVITIES.
package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var completed atomic.Int64

func Noop(ctx workflow.Context, activities int) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	for range activities {
		if err := workflow.ExecuteActivity(ctx, Work).Get(ctx, nil); err != nil {
			return err
		}
	}
	return nil
}

func Work(context.Context) error {
	completed.Add(1)
	return nil
}

func main() {
	activities := 1
	if v, err := strconv.Atoi(os.Getenv("ACTIVITIES")); err == nil && v >= 0 {
		activities = v
	}
	var calls, refused, polls atomic.Int64
	count := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		err := invoker(ctx, method, req, reply, cc, opts...)
		if strings.Contains(method, "/Poll") {
			polls.Add(1)
		} else {
			calls.Add(1)
		}
		if status.Code(err) == codes.ResourceExhausted {
			refused.Add(1)
		}
		return err
	}
	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		addr = "localhost:7233"
	}
	c, err := client.Dial(client.Options{
		HostPort: addr,
		ConnectionOptions: client.ConnectionOptions{
			DialOptions: []grpc.DialOption{grpc.WithChainUnaryInterceptor(count)},
		},
	})
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()

	w := worker.New(c, "bench", worker.Options{
		MaxConcurrentWorkflowTaskPollers: 8,
		MaxConcurrentActivityTaskPollers: 8,
	})
	// The burst starts "Noop" with no input; the wrapper supplies ACTIVITIES.
	w.RegisterWorkflowWithOptions(func(ctx workflow.Context) error { return Noop(ctx, activities) },
		workflow.RegisterOptions{Name: "Noop"})
	w.RegisterActivity(Work)
	go func() {
		for range time.Tick(10 * time.Second) {
			log.Printf("completed %d calls %d polls %d refused %d", completed.Load(), calls.Load(), polls.Load(), refused.Load())
		}
	}()
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatal(err)
	}
}
