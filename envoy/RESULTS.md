# Results: Envoy or Aquifer in front of an overloaded service

Fly, 2026-10-26. Backend and front door in `ord`, clients in `ord`, `iad`, `dfw` and `lhr`, Envoy v1.39.3, Aquifer `630501d`. Raw data is in `results/`; the setup is in the [README](README.md).

| Runs | Series | Harness image |
|---|---|---|
| Steady, Envoy and Aquifer (3 each) | `env` | `registry.fly.io/aqb-envoy-images:a3b903d` |
| Changing, Envoy (2) | `env` | `registry.fly.io/aqb-envoy-images:a3b903d` |
| Changing, Aquifer (3) | `env2` | `registry.fly.io/aqb-envoy-images:c81776d` |

**What this measures.** Work getting done under contention, with capacity that grows. It is not a fair test of instant responses: neither Envoy nor a queue answers instantly under contention, and Envoy is not designed for instant deadlines unless the service is over-provisioned. Interactive traffic belongs on a live connection (a WebSocket, a stream, or Aquifer's `/proxy`) to a backend with room to spare. The 2 s deadline is shown for completeness, not as a criterion.

**Runs left out.** Runs where the harness or setup was broken were removed (they remain in git history):

- `env` Aquifer changing: the backend sent its whole capacity as a fixed header to every queue, so Aquifer overloaded it.
- `env2` scale-out, all three setups: Envoy listed machines before they were up, and its default panic threshold then routed traffic to them; Aquifer's pool had no per-machine limit on requests in flight.
- Smoke and trial runs, and one run that failed in setup.

The scale-out comparison will be re-run once both are fixed.

## Steady: 0.5× load, capacity to spare

| | Envoy (3 runs) | Aquifer (3 runs) |
|---|---|---|
| Jobs completed | 100% (one missed its 2 s deadline, in `lhr`) | 100% |
| Requests per completed job | 1.00 (31–48 refused per run, then retried) | 1.00 (none refused) |
| Duplicated or orphaned work | 0 | 0 |
| Worst tenant p99, submit to result | 0.36–0.39 s | 0.36 s |
| Worst first-answer p99 | 0.36–0.38 s | 0.09 s |

A tie. With spare capacity neither front door costs anything. Envoy's handful of refusals were bursts above a tenant's token bucket.

## Changing: 3× spike, capacity doubles 15 minutes in

About 90,000 jobs offered per run; the backend could have done about 108,000 (40/s for 15 minutes, then 80/s). The backend sends live Aqueduct headers: its current capacity divided by the queues Aquifer reports, lower when it is overloaded. Clients retry a refused job up to 6 times, with exponential backoff (200 ms up to 20 s) and full jitter.

| | Envoy (2 runs) | Aquifer (3 runs) |
|---|---|---|
| Jobs completed | 52,800 (59%) | **59,900–60,100 (67%)** |
| Jobs given up after 6 tries | 32,300–32,400 | 29,600–30,000 |
| Jobs given up at the 2 s deadline (old rule, see below) | 4,700–4,800 | – |
| Retries per job offered | 2.4 | 2.0 |
| Requests per completed job | 5.8 | **4.5** |
| Backend errors | 0 | 0 |
| Work done for a caller that had already gone | 59–77 jobs | **0** |
| Duplicated executions | 0 | 0 |
| Small tenants' jobs completed | 84–88% | **99.9–100%** |
| Big tenant's jobs completed | 44% | 51–52% |
| Backend throughput, 8 then 16 workers (jobs/s) | 32–34, then 46–52 | 36, then 51 |
| Worst tenant p99, submit to result, completed jobs only | 7 s | 3.5–3.6 min |
| Deadline jobs answered within 2 s | 47% | 25–26% |

- **More work done, with fewer requests.** Aquifer completed about 7,200 more jobs per run (14%), with about a fifth fewer requests per completed job, and none of the backend's work was wasted on callers that had gone.
- **Fair.** Small tenants' jobs all completed, bar a handful. Under Envoy, 12–16% of theirs were given up.
- **Driven as hard as Envoy drove it.** With live headers the backend kept almost no queue of its own and ran at Envoy's throughput or better.
- **Latency.** Envoy's p99 counts only the jobs that got through; the 37,000 it turned away never finished, each after six attempts. Aquifer held work instead of turning it away, so its slowest completed jobs waited minutes, and it finished thousands of jobs Envoy dropped.
- **Both turned work away at the peak.** The spike offered more than the backend could do. Aquifer refuses at its front door once it holds about 60 s of the backend's capacity (the backend's `X-Aqueduct-Max-Backlog`); a longer backlog would complete more and wait longer.

Caveat: the Envoy runs come from `env`, where the client gave up on deadline jobs at 2 s, not just measured it (about 4,800 jobs per run, 5%). Some of those would have completed with more tries. Envoy changing has not been re-run under the measured deadline.

## What we can say

- When the service reports its load and Aquifer follows it, Aquifer completes more work than Envoy, with fewer requests, no wasted backend work and complete fairness to small tenants. Individual jobs wait longer, because it holds work instead of refusing it.
- A rate limit alone does not protect a service that slows down under load; it also needs a limit on requests in flight. Aquifer gets one from the backend's `X-Aqueduct-Max-Concurrent`, but its pools do not have one yet.

Next: a per-member concurrency limit for Aquifer pools (needs a design), then the scale-out regime with that and `healthy_panic_threshold: 0` for Envoy, and Envoy changing under the measured deadline.
