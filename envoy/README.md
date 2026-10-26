# Envoy or Aquifer in front of an overloaded service

When a service is overloaded, something has to say no. Envoy says no at once: a `429` or `503` that sends the work back to the caller, which retries later. Aquifer says "accepted", keeps the job, and sends it on when there is room, retrying for the caller and keeping each tenant's share fair. This benchmark measures what each approach costs: requests sent, work done twice, fairness between tenants, and how fast the work gets through.

Neither is assumed to win. Envoy is built for traffic between services you own and gives the lowest latency when there is spare capacity; Aquifer is built for the front door, where callers aren't yours and traffic changes. The two regimes below test both cases.

## Setup

- **Backend:** a synthetic service ([cmd/backend](cmd/backend/main.go)) with a fixed number of workers (8 at 200 ms: 40 jobs/s). Requests wait for a worker; past 64 waiting it answers `503` with `Retry-After: 1`. When the line is longer than the number of workers, work slows down (up to 3×). Work that has started always finishes, even after the caller gives up, and every execution is counted by job ID, so duplicated work is exact. It advertises its capacity in Aqueduct's headers (`X-Aqueduct-Rps`, `X-Aqueduct-Max-Concurrent`); Envoy ignores them.
- **Envoy** ([envoy.yaml.tmpl](deploy/envoy.yaml.tmpl)), tuned as a careful team would: per-tenant local rate limits with `Retry-After`, admission control, adaptive concurrency, circuit breakers with a pending queue, and a retry budget on Envoy's own retries.
- **Aquifer** with its defaults; per-tenant queues and fair admission are on by default.
- **Same ceilings for both:** 80 requests/s and 32–64 in flight, twice the starting capacity, so the scale-up below is usable by both.
- **Clients** ([cmd/client](cmd/client/main.go)) in four regions retry like a good SDK: up to 6 attempts, exponential backoff with full jitter, never sooner than `Retry-After`. Against Envoy that applies to every job; against Aquifer only to a refused submission. 10% of jobs carry a 2 s deadline.

| Tenant | Jobs/s at 1× | Region |
|---|---|---|
| big | 20 | `ord`, next to the backend |
| medium | 10 | `iad` |
| small-1 | 2.5 | `iad` |
| small-2, small-3 | 2.5 each | `dfw` |
| small-4 | 2.5 | `lhr` |

1× is 40 jobs/s, the backend's starting capacity. The backend and its front door run in `ord`.

## Regimes

- **steady:** 20 minutes at 0.5×. The backend has twice the capacity it needs, the case where Envoy should do best.
- **changing:** 5 min at 0.5×, a 5 min spike at 3×, 10 min at 1.5×, 10 min at 0.5×. Capacity doubles to 16 workers 15 minutes in, while still overloaded, like an autoscaler catching up late.

## What is measured

| Claim | Measure |
|---|---|
| Cheaper | requests sent per completed job; refused requests; backend executions per unique job (duplicates) |
| Faster | completion latency per tenant and phase; time to first answer; deadline jobs answered in time |
| More reliable | jobs that never completed, and why |
| Fair | each tenant's completed share against its offered share over time |

## Running

```sh
FLY_ORG=... IMAGE=ghcr.io/rjpruitt16/aqueduct-benchmarks-envoy:<commit> deploy/run.sh envoy changing env-c1
```

Results land in `results/<run-name>/`: each client's report (`clients/*.json`), the backend's 10-second samples (`backend.jsonl`) and totals (`backend-summary.json`), Envoy's stats or Aquifer's metrics, and the configuration used.
