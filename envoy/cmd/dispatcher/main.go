// Command dispatcher runs the jobs the bottom services queue in Redis, for the
// wild regime's envoy-queue setup: the queue a team would build itself to
// hold work instead of refusing it, with Envoy kept in front of the backend.
//
// Each tenant has a list, q:<tenant>; the services push a job onto it once
// (cmd/service). The dispatcher takes jobs round-robin across tenants, with a
// blocking pop whose list order starts at the next tenant each time, and sends
// each one to the backend through Envoy's /work. It keeps as many in flight as
// the backend advertises in X-Aqueduct-Max-Concurrent, the same live header
// Aquifer follows. A refusal puts the job back at the head of its list and
// pauses dispatching for Retry-After (default 1 s). A finished job's result is
// stored and published on done:<job>, which wakes every call waiting for it.
//
// Counts are POSTed to REPORT_URL/svc-dispatcher every 10 s, with each
// tenant's queue length.
//
// Configuration (environment, or the run configuration at CONFIG_URL):
//
//	REDIS_URL     redis://host:port
//	FRONT_URL     Envoy's base URL
//	MAX_INFLIGHT  in flight until the backend says otherwise (default 8)
//	DISPATCH_TIMEOUT  per request (default 30s)
//	START_AT      Unix seconds the run starts, for the report's intervals
//	REPORT_URL    where to POST the counts
//	CONFIG_URL    wait for the run's settings there
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type dispatcher struct {
	rdb   *redis.Client
	front string
	http  *http.Client

	mu       sync.Mutex
	cond     *sync.Cond
	limit    int
	inflight int
	paused   time.Time
	tenants  []string
	next     int
	counts   counts
	interval map[string]int
}

type counts struct {
	Taken       int            `json:"taken"`
	Sent        int            `json:"sent"`
	Completed   int            `json:"completed"`
	Requeued    int            `json:"requeued"`
	Failures    map[string]int `json:"failures"`
	MostLimit   int            `json:"most_in_flight_allowed"`
	SentByTen   map[string]int `json:"sent_by_tenant"`
	DoneByTen   map[string]int `json:"completed_by_tenant"`
	RedisErrors int            `json:"redis_errors"`
}

func main() {
	if u := os.Getenv("CONFIG_URL"); u != "" {
		loadConfig(u)
	}
	opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		log.Fatalf("REDIS_URL: %v", err)
	}
	opt.PoolSize = 256
	timeout := envDur("DISPATCH_TIMEOUT", 30*time.Second)
	d := &dispatcher{
		rdb:      redis.NewClient(opt),
		front:    strings.TrimRight(os.Getenv("FRONT_URL"), "/"),
		http:     &http.Client{Timeout: timeout, Transport: &http.Transport{MaxIdleConnsPerHost: 256}},
		limit:    envInt("MAX_INFLIGHT", 8),
		counts:   counts{Failures: map[string]int{}, SentByTen: map[string]int{}, DoneByTen: map[string]int{}},
		interval: map[string]int{},
	}
	d.cond = sync.NewCond(&d.mu)
	d.counts.MostLimit = d.limit
	if u := os.Getenv("REPORT_URL"); u != "" {
		go d.report(strings.TrimRight(u, "/") + "/svc-dispatcher")
	}
	go d.refreshTenants()
	log.Printf("dispatcher: front %s, %d in flight to start", d.front, d.limit)
	d.run()
}

// run takes jobs while there is room in flight and dispatching isn't paused.
func (d *dispatcher) run() {
	ctx := context.Background()
	for {
		d.mu.Lock()
		for d.inflight >= d.limit {
			d.cond.Wait()
		}
		wait := time.Until(d.paused)
		keys := d.order()
		d.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
			continue
		}
		if len(keys) == 0 {
			time.Sleep(time.Second)
			continue
		}
		got, err := d.rdb.BLPop(ctx, time.Second, keys...).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			d.mu.Lock()
			d.counts.RedisErrors++
			d.mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		tenant, job := strings.TrimPrefix(got[0], "q:"), got[1]
		// A refusal can pause dispatching while BLPop is waiting; honor it
		// before sending what it returned.
		d.mu.Lock()
		wait = time.Until(d.paused)
		d.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
		}
		d.mu.Lock()
		d.counts.Taken++
		d.inflight++
		// The next pop starts after this tenant, so each gets its turn.
		for i, t := range d.tenants {
			if t == tenant {
				d.next = i + 1
			}
		}
		d.mu.Unlock()
		go d.send(tenant, job)
	}
}

// order is the tenants' lists, starting at the one whose turn it is. d.mu
// must be held.
func (d *dispatcher) order() []string {
	n := len(d.tenants)
	keys := make([]string, 0, n)
	for i := range n {
		keys = append(keys, "q:"+d.tenants[(d.next+i)%n])
	}
	return keys
}

// send runs one job through Envoy, then stores its result or puts it back.
func (d *dispatcher) send(tenant, job string) {
	ctx := context.Background()
	defer func() {
		d.mu.Lock()
		d.inflight--
		d.cond.Signal()
		d.mu.Unlock()
	}()
	d.mu.Lock()
	d.counts.Sent++
	d.counts.SentByTen[tenant]++
	d.interval["sent"]++
	d.mu.Unlock()
	req, _ := http.NewRequest(http.MethodPost, d.front+"/work", nil)
	req.Header.Set("X-Tenant", tenant)
	req.Header.Set("X-Job-Id", job)
	resp, err := d.http.Do(req)
	why, retryAfter := "", time.Second
	if err != nil {
		why = "error"
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if v, err := strconv.Atoi(resp.Header.Get("X-Aqueduct-Max-Concurrent")); err == nil && v > 0 {
			d.mu.Lock()
			d.limit = v
			d.counts.MostLimit = max(d.counts.MostLimit, v)
			d.cond.Broadcast()
			d.mu.Unlock()
		}
		if v, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && v > 0 {
			retryAfter = time.Duration(v) * time.Second
		}
		if resp.StatusCode != http.StatusOK {
			why = fmt.Sprintf("http_%d", resp.StatusCode)
		}
	}
	if why == "" {
		// The job left its list in BLPop: keep trying until its result is
		// stored and its waiters are woken, or they wait out their timeouts.
		for {
			_, err := d.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
				p.Set(ctx, "result:"+job, "ok", time.Hour)
				p.Publish(ctx, "done:"+job, "ok")
				return nil
			})
			if err == nil {
				break
			}
			d.mu.Lock()
			d.counts.RedisErrors++
			d.mu.Unlock()
			time.Sleep(100 * time.Millisecond)
		}
		d.mu.Lock()
		d.counts.Completed++
		d.counts.DoneByTen[tenant]++
		d.interval["completed"]++
		d.mu.Unlock()
		return
	}
	// Refused or failed: back to the head of its list, and wait before the
	// next dispatch.
	// Pause first, so the job isn't taken again before Retry-After.
	d.mu.Lock()
	if until := time.Now().Add(retryAfter); until.After(d.paused) {
		d.paused = until
	}
	d.mu.Unlock()
	// The job exists only here now: keep trying to put it back, or it is lost
	// while its queued: marker blocks requeueing it for an hour.
	for d.rdb.LPush(ctx, "q:"+tenant, job).Err() != nil {
		d.mu.Lock()
		d.counts.RedisErrors++
		d.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	d.mu.Lock()
	d.counts.Failures[why]++
	d.counts.Requeued++
	d.mu.Unlock()
}

// refreshTenants keeps the list of tenants that have queued work, every second.
func (d *dispatcher) refreshTenants() {
	for {
		ts, err := d.rdb.SMembers(context.Background(), "tenants").Result()
		if err == nil {
			sort.Strings(ts)
			d.mu.Lock()
			d.tenants = ts
			if d.next >= len(ts) {
				d.next = 0
			}
			d.mu.Unlock()
		}
		time.Sleep(time.Second)
	}
}

// report POSTs the counts every 10 s, with each tenant's queue length.
func (d *dispatcher) report(url string) {
	start := time.Now()
	if v, err := strconv.ParseInt(os.Getenv("START_AT"), 10, 64); err == nil {
		start = time.Unix(v, 0)
	}
	time.Sleep(time.Until(start))
	intervals := []map[string]any{}
	for range time.Tick(10 * time.Second) {
		d.mu.Lock()
		tenants := append([]string(nil), d.tenants...)
		d.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		queued := map[string]int64{}
		for _, t := range tenants {
			queued[t], _ = d.rdb.LLen(ctx, "q:"+t).Result()
		}
		info, _ := d.rdb.Info(ctx, "stats", "commandstats", "cpu", "memory").Result()
		cancel()
		d.mu.Lock()
		intervals = append(intervals, map[string]any{"t": int(time.Since(start).Seconds()), "sent": d.interval["sent"],
			"completed": d.interval["completed"], "in_flight": d.inflight, "limit": d.limit, "queued": queued})
		d.interval = map[string]int{}
		raw, _ := json.Marshal(map[string]any{"name": "dispatcher", "counts": d.counts, "intervals": intervals, "redis_info": info})
		d.mu.Unlock()
		if resp, err := control.Post(url, "application/json", bytes.NewReader(raw)); err == nil {
			resp.Body.Close()
		}
	}
}

// control carries the run's own traffic, bounded so a stuck peer can't hang it.
var control = &http.Client{Timeout: 10 * time.Second}

// loadConfig polls url until it returns the run's settings, then sets them
// in the environment. Settings already in the environment win.
func loadConfig(url string) {
	for {
		resp, err := control.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			var cfg map[string]string
			err = json.NewDecoder(resp.Body).Decode(&cfg)
			resp.Body.Close()
			if err == nil {
				for k, v := range cfg {
					if os.Getenv(k) == "" {
						os.Setenv(k, v)
					}
				}
				return
			}
		} else if err == nil {
			resp.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envDur(name string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return v
	}
	return def
}
