# Envoy or Aquifer in front of an overloaded service

When a service is overloaded, something has to say no. Envoy says no at once: a `429` or `503` that sends the work back to the caller, which retries later. Aquifer says "accepted", keeps the job, and sends it on when there is room, retrying for the caller and keeping each tenant's share fair. This benchmark measures what each approach costs: requests sent, work done twice, fairness between tenants, and how fast the work gets through.

Neither is assumed to win. Envoy is built for traffic between services you own and gives the lowest latency when there is spare capacity; Aquifer is built for the front door, where callers aren't yours and traffic changes. The two regimes below test both cases.

## Setup

- **Backend:** a synthetic service ([cmd/backend](cmd/backend/main.go)) with a fixed number of workers (8 at 200 ms: 40 jobs/s). Requests wait for a worker; past 64 waiting it answers `503` with `Retry-After: 1`. When the line is longer than the number of workers, work slows down (up to 3×). Work that has started always finishes, even after the caller gives up, and every execution is counted by job ID, so duplicated work is exact, also across machines. It reports its load in Aqueduct's headers, which Envoy ignores (see [Live headers](#live-headers)).
- **Envoy** ([envoy.yaml.tmpl](deploy/envoy.yaml.tmpl)), tuned as a careful team would: per-tenant local rate limits with `Retry-After`, admission control, adaptive concurrency, circuit breakers with a pending queue, and a retry budget on Envoy's own retries.
- **Envoy tuning notes.** Two settings matter for a fair result. Adaptive concurrency holds its limit at `min_concurrency` while it re-measures the backend's best latency, so that is set to the concurrency normal traffic needs (8); at 4 it blocked ordinary bursts. And it refuses with `429` instead of its default `503`, because admission control counts every `5xx` it sees as a backend failure and otherwise rejected more traffic in response to adaptive concurrency's own refusals. Both were found in trial runs.
- **Aquifer** with its defaults; per-tenant queues and fair admission are on by default. In the scale-out regime jobs go to an Aquifer pool that the backend machines register in.
- **Envoy in front of Aquifer** (`both`): Envoy at the edge with per-tenant rate limits at twice the backend share, as a guard against abuse, forwarding submissions to Aquifer ([envoy-edge.yaml.tmpl](deploy/envoy-edge.yaml.tmpl)). This is how a team that already runs Envoy would add Aquifer.
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

- **chains:** a different workload, where no job is given up. Each tenant works in lanes; a lane runs a chain of four dependent jobs (A, then B and C in parallel, then D), and a refused step is retried until it succeeds, holding up the rest of the chain. A lane starts a new chain every 4 s, or as soon as the last one ends if it took longer. 5 min at 0.5×, 15 min at 3×, 10 min at 2×, 10 min at 0.5×; capacity doubles 20 minutes in. One client machine per tenant:

  | Tenant | Region | Retries | Jobs/s at 1× |
  |---|---|---|---|
  | big | `ord` | polite | 16 |
  | near-polite, near-aggr | `ord` | polite, aggressive | 4 each |
  | far-polite, far-aggr | `lhr` | polite, aggressive | 4 each |
  | vfar-polite, vfar-aggr | `syd` | polite, aggressive | 4 each |

  Polite tenants back off exponentially with full jitter (200 ms, doubling up to 60 s) and honor `Retry-After`. Aggressive tenants retry every 100 ms and ignore it. Under Aquifer a job is submitted once and Aquifer holds it until it runs.

- **wild:** retries stacked across a chain of services. A client request goes `api` → `orders` → `inventory` and `payments` in parallel → the front door → the backend, and every service tries 3 times before passing a 503 up. 15 tenants in `ord`, `lhr` and `syd` (one large and four small per region) retry until they succeed, in four styles: polite, no-jitter, aggressive, and stacked (library inside SDK inside app). 40 minutes with a 3× peak, seven bursts, a 60 s backend outage at 20 minutes, and capacity doubling at 25. See [DESIGN-wild.md](DESIGN-wild.md).
- **diy:** the wild regime with Envoy plus what a team would build itself: idempotency keys in Redis (`envoy-idem`), and idempotency plus a fair per-tenant queue and dispatcher (`envoy-queue`). See [DESIGN-diy.md](DESIGN-diy.md).

## Live headers

From the `env2` series on, every backend reports its load the way Aquifer expects a service to. Aquifer sends `X-Aqueduct-Active-Queues` on each request, the number of its queues sharing this service, and the backend answers with each queue's share:

- `X-Aqueduct-Rps`: capacity (workers ÷ service time) ÷ active queues, scaled down by workers ÷ (busy + waiting) once requests wait, so a line as long as the workers halves it.
- `X-Aqueduct-Max-Concurrent`: workers ÷ active queues, rounded up.
- `X-Aqueduct-Max-Backlog`: 60 seconds of capacity, the most work Aquifer should hold for it.

In a pool, members report capacity by registering instead (every 2 s, with the same live capacity), since a pool's queues share one pace across members.

The first series (`env`) sent the full, fixed capacity on every response; see [RESULTS.md](RESULTS.md) for what that did.

## Scale-out regime

The changing regime's load, but the capacity comes from new machines: one backend machine (8 workers) at the start, and two more 15 minutes in, one as big (8 workers) and one half the size (4). They run from the start but stay off the network until that moment, so it is exact and the same for every front door, without Fly's machine start times in the result. Envoy lists all three machines and finds the new ones through its health checks (every 2 s); Aquifer gets them when they register in its pool. Both ceilings are 100 requests/s, the capacity after the scale-out. Each machine records when it came online and every job it ran.

## What is measured

| Claim | Measure |
|---|---|
| Cheaper | requests sent per completed job; refused requests; backend executions per unique job (duplicates) |
| Faster | completion latency per tenant and phase; time to first answer; deadline jobs answered in time |
| More reliable | jobs that never completed, and why |
| Fair | each tenant's completed share against its offered share over time |

## Running

From `envoy/`, a whole series (three rounds; each round runs its runs side by side):

```sh
FLY_ORG=... IMAGE=... deploy/series.sh env
FLY_ORG=... IMAGE=... RUNS="aquifer:changing envoy:scaleout aquifer:scaleout both:scaleout" deploy/series.sh env2
```

One run:

```sh
FLY_ORG=... IMAGE=ghcr.io/rjpruitt16/aqueduct-benchmarks-envoy:<commit> deploy/run.sh envoy changing env-c1
```

Results land in `results/<run-name>/`: each client's report (`clients/*.json`), the backend's 10-second samples (`backend.jsonl`) and totals (`backend-summary.json`), Envoy's stats or Aquifer's metrics, and the configuration used.
