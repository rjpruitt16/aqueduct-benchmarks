# Results: Envoy or Aquifer in front of an overloaded service

Fly, 2026-10-26. Backend and front door in `ord`, clients in `ord`, `iad`, `dfw` and `lhr`, Envoy v1.39.3, Aquifer `630501d`. Raw data is in `results/`; the setup is in the [README](README.md).

| Runs | Series | Harness image |
|---|---|---|
| Steady, Envoy and Aquifer (3 each) | `env` | `registry.fly.io/aqb-envoy-images:a3b903d` |
| Changing, Envoy (2) | `env` | `registry.fly.io/aqb-envoy-images:a3b903d` |
| Changing, Aquifer (3) | `env2` | `registry.fly.io/aqb-envoy-images:c81776d` |
| Chains, Envoy and Aquifer (3 each) | `chains` | `registry.fly.io/aqb-envoy-images:cd70070` |

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
- **Latency.** Envoy's p99 counts only the jobs that got through; the 37,000 it turned away never finished (32,000 after six attempts, 4,800 at the 2 s deadline). Aquifer held work instead of turning it away, so its slowest completed jobs waited minutes, and it finished thousands of jobs Envoy dropped.
- **Both turned work away at the peak.** The spike offered more than the backend could do. Aquifer refuses at its front door once it holds about 60 s of the backend's capacity (the backend's `X-Aqueduct-Max-Backlog`); a longer backlog would complete more and wait longer.

Caveat: the Envoy runs come from `env`, where the client gave up on deadline jobs at 2 s, not just measured it (about 4,800 jobs per run, 5%). Some of those would have completed with more tries. Envoy changing has not been re-run under the measured deadline.

## Chains: nothing is given up, everyone retries until it works

The question here is what overload costs when no caller gives up. Every tenant runs chains of four dependent jobs and retries a refused step until it succeeds, so a refusal turns into waiting instead of a lost job. Six tenants have the same demand and differ only in region (`ord`, `lhr`, `syd`) and in how they retry (polite exponential backoff, or aggressive retries every 100 ms); a seventh, `big`, is four times their size. Load reaches 3×, and capacity doubles 20 minutes in. See the [README](README.md#regimes) for the setup.

All six runs are clean: every job completed, none were left open, and the backend returned no errors.

| | Envoy (3 runs) | Aquifer (3 runs) |
|---|---|---|
| Chains completed | 21,700–22,000 | **22,700** |
| Requests sent | 306,000–311,000 | **90,800–90,900** |
| Requests per completed job | 3.5 | **1.0** |
| Peak requests per second at the front door | 207–368 | **52–60** |
| Most tries for one job | 195–234 | **1** |
| Time clients spent backing off, all tenants | about 30 hours per run | **0** |
| Chains completed, each same-size tenant | aggressive 3,000–3,300, polite 2,000–2,200 | **2,850–2,900, all six** |
| Slowest chains (p99), same-size tenants | aggressive 14–17 s, polite 1.9–2.8 min | **9 s, all six** |
| Slowest chains (p99), `big` | 2.8–2.9 min | **36 s** |
| Backend share, each same-size tenant | aggressive 14–15%, polite 9–10% | **13%, all six** |
| Backend share, `big` | 28–29% | 24% |
| Duplicated or orphaned work | 1 duplicate in one run | 0 |

- **Under Envoy, retrying harder wins.** Tenants with the same demand did 50% more work when they retried aggressively, and their slowest chains took seconds where polite tenants' took minutes. Being polite was punished. Region barely mattered; retry style did.
- **Under Aquifer, how a tenant retries does not matter.** Each job was sent once and held until it ran. The six same-size tenants got the same share and the same latency, wherever they were.
- **The same backend did slightly more with far less traffic.** Aquifer completed about 4% more chains while the front door saw under a third of the requests and about a fifth of the peak rate. Under Envoy, roughly 70% of all requests were refused retries.
- **The big tenant.** Aquifer splits capacity evenly between queues, so `big`, with four times the demand, got 24% of the backend instead of 28% and completed about 13% fewer chains than under Envoy. Its slowest chains still finished five times sooner (36 s against nearly 3 minutes), because it never sat in backoff.

**What this says, simply.** When callers must eventually get their work done, refusing them does not reduce the work. It only adds requests and waiting, and it hands capacity to whoever retries hardest. A queue that holds the work does the same job with one request each, and shares capacity by tenant, not by how aggressive its client code is.

Round 3 note: the Fly access token on the orchestrating machine expired after both round 3 runs had finished and all clients had reported. Their results were copied and the apps destroyed by hand with the same commands `run.sh` uses, so `run.txt` for round 3 was written by hand, with the load start taken from the backend's samples.

## What we can say

- When the service reports its load and Aquifer follows it, Aquifer completes more work than Envoy, with fewer requests, no wasted backend work and complete fairness to small tenants. Individual jobs wait longer, because it holds work instead of refusing it.
- When callers retry until their work is done, Envoy's refusals turn into retries: three and a half requests per job, with capacity going to the tenants that retry hardest. Aquifer did slightly more work with one request per job and gave same-size tenants the same share and latency, whatever their region or retry style. The cost is that a much bigger tenant gets an even split rather than the larger share it can grab by volume.
- A rate limit alone does not protect a service that slows down under load; it also needs a limit on requests in flight. Aquifer gets one from the backend's `X-Aqueduct-Max-Concurrent`, but its pools do not have one yet.

Next: a per-member concurrency limit for Aquifer pools (needs a design), then the scale-out regime with that and `healthy_panic_threshold: 0` for Envoy, and Envoy changing under the measured deadline.
