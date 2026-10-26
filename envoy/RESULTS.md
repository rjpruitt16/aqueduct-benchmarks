# Results: Envoy or Aquifer in front of an overloaded service

Series `env`, 2026-10-26, Fly. Backend and front door in `ord`, clients in `ord`, `iad`, `dfw` and `lhr`, Envoy v1.39.3, Aquifer `630501d`, harness image `registry.fly.io/aqb-envoy-images:a3b903d`. Three rounds of four runs side by side. Round 1's Envoy changing run failed in setup (a Fly ssh call from the orchestrator, before load started), so the changing regime has two Envoy runs and three Aquifer runs. Raw data is in `results/env-*`; the setup is in the [README](README.md).

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

About 90,000 jobs offered per run; the backend could have done about 108,000 (40/s for 15 min, then 80/s).

| | Envoy-2 | Envoy-3 | Aquifer-1 | Aquifer-2 | Aquifer-3 |
|---|---|---|---|---|---|
| Jobs completed | 52,817 | 52,804 | 45,622 | 45,395 | 45,319 |
| Requests per completed job (client to front door) | 5.80 | 5.77 | 6.28 | 6.27 | 6.39 |
| Backend `503`s (front door to backend) | 0 | 0 | 34,414 | 36,211 | 35,797 |
| Duplicated executions | 0 | 0 | 0 | 0 | 0 |
| Orphaned work (caller gone before it finished) | 77 | 59 | 0 | 0 | 0 |
| Worst tenant p99, submit to result | 7.2 s | 7.2 s | 1,190 s | 1,180 s | 1,188 s |
| Deadline jobs answered within 2 s | 47% | 47% | 13% | 14% | 13% |
| Small tenants' jobs completed | 84–85% | 86–88% | 90–92% | 91–92% | 89–92% |
| Big tenant's jobs completed | 44% | 44% | 33% | 33% | 33% |

Backend throughput, jobs/s (the same in every run of a front door):

| | 8 workers, overloaded | 16 workers, overloaded |
|---|---|---|
| Envoy | 32–34 | 46–52 |
| Aquifer | 13.5 | 32 |

**Envoy wins this regime clearly.** It completed 16% more jobs, its tail latency was seconds rather than 20 minutes, and it answered more than three times as many deadline jobs in time. Its cost is retries: each completed job took about 5.8 requests. In Envoy-2, 225,807 were refused by the per-tenant rate limit and 27,487 by adaptive concurrency. Admission control refused nothing once adaptive concurrency answered `429`.

**Aquifer lost because the backend in this series spoke Aqueduct badly, so Aquifer overloaded it.** The backend sent the same headers on every response, idle or drowning: `X-Aqueduct-Max-Concurrent` 8 (then 16) and `X-Aqueduct-Rps` 40 (then 80), its whole capacity. Aquifer applies those values per queue and tells the service how many of its queues share it (`X-Aqueduct-Active-Queues` on every request), so a service is meant to send each queue its share: the convention Aquifer itself follows when it paces its own callers. With six tenant queues each taking the whole number, Aquifer kept 48 requests in flight against 8 workers (then 96 against 16). The backend thrashed at its 3× slowdown, and after the scale-up it also refused with `503` about 60 times a second. That explains the whole result: throughput, latency and missed deadlines. It was a harness choice, not a measure of Aquifer with a backend that reports its load: Envoy got its adaptive filter, Aquifer got a fixed number. The series below re-runs Aquifer against a backend that sends its live, per-caller capacity.

**Fairness held for both.** Each front door protected the small tenants from the big one: Envoy through per-tenant token buckets, Aquifer through per-tenant queues and fair admission. Aquifer completed a slightly larger share of the small tenants' work; Envoy completed it much sooner.

**No duplicated work either way.** Envoy left 59–77 jobs running after the client gave up (the 10 s timeout), work the backend did for nobody. Aquifer left none.

## What this does not show

- Aquifer against a backend that reports its load. The `env2` series below does.
- Envoy's throughput after the scale-up was capped by its own per-tenant limits (20/s each), not by the backend. A team would raise those limits on scaling; Envoy has no signal to do it on its own.
- Capacity coming from new machines. Here one machine got more workers at the same address, which needs no discovery from either front door. The `env2` series adds machines.

# Series `env2`: live headers and machines joining

2026-10-26, Fly, same clients, regions and phases as `env`. Harness image `registry.fly.io/aqb-envoy-images:c81776d`. Three rounds of four runs side by side, all 12 completed. Raw data is in `results/env2-*`.

What changed from `env`:

- The backend sends live Aqueduct headers: its current capacity divided by the `X-Aqueduct-Active-Queues` Aquifer reports, smaller when it is overloaded.
- No client gives up at 2 s. The deadline is measured, not enforced.
- A scale-out regime: one 8-worker machine for 15 minutes, then a second 8-worker and a 4-worker machine come online, each one recording when it starts listening. Envoy load-balances across all three (least request, active health checks). Aquifer puts them in a pool that members join by registering their capacity. A third setup puts an Envoy edge in front of Aquifer.

**This is not a fair test of instant responses.** It measures work getting done under contention, with capacity that grows. Neither Envoy nor a queue answers instantly under contention unless there is capacity to spare: Envoy was not designed for instant deadlines without over-provisioning, and a queue trades waiting for completion. Interactive traffic belongs on a live connection (a WebSocket, a stream, or Aquifer's `/proxy`) to a backend with room to spare. The 2 s column below is shown for completeness, not as a criterion. The measures that matter here are how much work got done, how long it took, how many requests it cost, duplicated or wasted work, and fairness.

## Changing (one machine doubles its workers), Aquifer with live headers

| | Envoy (`env`, 2 runs) | Aquifer, static headers (`env`, 3 runs) | Aquifer, live headers (`env2`, 3 runs) |
|---|---|---|---|
| Jobs completed (of about 90,000) | 52,800 | 45,300–45,600 | **59,900–60,100** |
| Requests per completed job | 5.8 | 6.3–6.4 | 4.4–4.5 |
| Backend `503`s | 0 | 34,000–36,000 | **0** |
| Small tenants' jobs completed | 84–88% | 89–92% | **100%** |
| Worst tenant p99, submit to result | 7 s | about 20 min | 3.5–3.6 min |
| Deadline jobs answered within 2 s | 47% | 13–14% | 25–26% |
| Backend throughput, 8 then 16 workers (jobs/s) | 32–34, then 46–52 | 13.5, then 32 | 36, then 51 |

With headers that report its load, the backend never built a queue of its own (about 0–1 requests waiting on average) and Aquifer drove it as fast as Envoy did. Aquifer completed about 14% more jobs than Envoy, every small tenant's job, and refused nothing at the backend. The cost is waiting: work queued in Aquifer instead of being refused, so the slowest jobs took minutes, where Envoy's took seconds and the rest were refused.

Caveat: Envoy's changing runs are from `env`, where the client gave up on deadline jobs at 2 s (about 10% of jobs). Envoy was not re-run with the measured deadline.

## Scale-out: both setups were flawed

The three runs of each setup agree closely, but each front door was held back by a configuration problem, so the scale-out numbers are not a comparison.

| | Envoy | Aquifer pool | Envoy edge + Aquifer pool |
|---|---|---|---|
| Jobs completed (of about 90,000) | 37,800–37,900 (42%) | 45,800–46,200 (51%) | 45,800–46,300 (51%) |
| Requests per completed job | 10.2 | 7.1 | 7.2–7.3 |
| Small tenants' jobs completed | 59–62% | 94–96% | 93–95% |
| Duplicated executions | 0 | 0 | 0 |
| Work split, 8-worker : 4-worker new machine | about 1 : 1 | about 2 : 1 | about 2 : 1 |
| New machines' first work after coming online | within 3–4 s | within 14 s | within 3–12 s |

The backend recorded work every 10 s, so the time to first work is accurate to about 10 s.

**Envoy was in panic mode.** By default Envoy stops trusting health checks when fewer than half of a cluster's hosts are healthy, and then spreads traffic across every host. For the first 15 minutes only one of the three listed machines was up, so Envoy sent most requests to machines that were not listening. Its stats show 91,800 requests routed in panic mode, 80,800 failed connections and 45,300 `503`s. Even at half load, before the spike, the one live machine did 8 jobs/s of the 20 offered, and clients got answers in 24 ms (p50), far faster than the backend's 200 ms service time: those were refusals. Setting `healthy_panic_threshold` to 0, or adding machines to the cluster only once they are up, would avoid this. Envoy did use the new machines within seconds once they were up, but split work evenly by request count, so the 4-worker machine got as much as an 8-worker one.

**Aquifer's pool had no concurrency limit per machine.** A pool member reports its capacity in jobs per second, and Aquifer shares that rate across the pool. It applies concurrency only per tenant queue, from the configuration (32 per queue here), and a pooled backend sends no headers. When the backend slowed down under overload, the same rate meant more requests in flight: each 8-worker machine had about 60 requests waiting and did 13–15 jobs/s, where the same machine with live headers did 36–51. A rate limit alone does not protect a service that slows down under load; it needs a limit on requests in flight too. Aquifer spread the work in proportion to capacity (2 : 1) and used the new machines within seconds, but overloaded every one of them.

**Envoy in front of Aquifer changed nothing** except adding an abuse guard: the edge refused about 212,000 requests over the big tenant's limit, and completion, latency and fairness matched Aquifer alone. Using them together costs nothing measurable.

## What we can say

- When the service reports its load and Aquifer listens, Aquifer gets more work done than Envoy, refuses nothing at the backend, and is completely fair to small tenants. It is slower per job, because it holds work instead of refusing it.
- A rate is not enough. Both Aquifer failures here (static headers in `env`, pools in `env2`) came from overloading a backend that slows down when overloaded. Whatever paces work needs a signal about requests in flight.
- Aquifer pools need a per-member concurrency limit before a scale-out comparison means anything.
- Envoy's defaults are unfriendly to machines that are listed before they are up.

Next: re-run scale-out with `healthy_panic_threshold: 0` for Envoy and a concurrency limit for Aquifer pool members (a change to Aquifer, still to be designed), and re-run Envoy changing with the measured deadline.
