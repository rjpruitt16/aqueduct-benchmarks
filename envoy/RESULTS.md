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
