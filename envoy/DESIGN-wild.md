# Design: the wild regime

Status: signed off and built (`deploy/run.sh wild`, `cmd/service`).

## Why

The chains regime showed Envoy needing 3.5 requests per job when callers retry until their work is done. Those clients were tidy: one retry loop each, random jitter, no timeouts, one hop to the backend. Real traffic is messier, and the mess multiplies retries. This regime adds the common ways it does, to measure how far the multiplier grows and what it costs the backend.

The hypothesis: under overload with cascading retries, Envoy's requests per job grow well past 3.5, the backend runs duplicate and abandoned work, and recovery after an outage takes longer. Aquifer's stay near one request per job, because nothing in the chain is refused and retried. It is a hypothesis; the runs may say otherwise.

## Topology

A chain of microservices, each calling the next and each retrying what it calls, with the shared, overloaded backend at the bottom:

```
clients ──> api ──> orders ──┬──> inventory ──┐
                             └──> payments ───┴──> front door ──> backend
                                                (Envoy or Aquifer)
```

- **Services** (new, all in `ord`, one small binary started with different roles):
  - `api` calls `orders` once.
  - `orders` calls `inventory` and `payments` in parallel and answers when both are done.
  - `inventory` and `payments` each run one backend job through the front door.
  - So one client request needs two backend jobs, three hops down.
- **Cascading retries.** Every service tries the one below it 3 times, 50 and 100 ms apart, with no jitter, then returns 503 to its caller, which retries in turn. Worst case per backend job, per client attempt: 3 (`api`) × 3 (`orders`) × 3 (`inventory`/`payments`) = 27 requests at the front door, before the client's own retries. This is the retry storm that takes down real systems, and each layer's retries are reasonable on their own.
- **Timeouts at every hop.** Each service gives up on the one below after a timeout (`api` 10 s, `orders` 8 s, leaves 5 s) and retries, while the abandoned call may still be running underneath. That is where abandoned and duplicate work comes from.
- **The front door sits only in front of the backend**, the one shared resource that runs out, which is where a team would put Envoy or Aquifer. The services between are plain HTTP in both setups, so the only difference is what happens at the bottom:
  - Envoy mode: the backend's overload comes back as 429s and 503s, and every layer above multiplies them.
  - Aquifer mode: `inventory` and `payments` submit their job to Aquifer and wait for the result (`/proxy`). While Aquifer has room nothing is refused, so nothing above retries. When its backlog is full it refuses at submit, and the same retry layers apply.
- Each service counts the requests it receives and makes, so the amplification can be read at every hop, not only at the bottom.

## Client behavior

Each tenant runs 10 concurrent workers (large tenants 40), each with its own retry state, so retries come from many independent sources, not one tidy loop per tenant. A worker sends one request at a time and waits until it succeeds before sending the next. Behaviors are assigned per tenant, so a tenant's workers all behave alike and tenants can be compared by behavior.

| Behavior | What it does |
|---|---|
| Polite | exponential backoff with full jitter (200 ms doubling to 60 s), honors `Retry-After` |
| No jitter | the same backoff without jitter, so workers that fail together retry together, in waves |
| Aggressive | retries every 100 ms, ignores `Retry-After` |
| Library stack | an HTTP library's 3 quick tries (50 ms apart) inside an SDK's 3 tries (1 s and 2 s apart) inside the app's polite loop |

Each region has one small tenant of each behavior, so behaviors can be compared with region held fixed; the large tenants are polite (`ord`), no jitter (`lhr`) and library stack (`syd`).

All of them also have a **timeout**: after 15 s (decided) without an answer they abandon the request and send it again, while the first copy may still be running. Every resend, at every level, carries the same request ID, and the bottom services name their job after it. In Aquifer mode that ID is the job's idempotency key, as Aquifer's API asks callers to do, so a resend joins the job already held; Envoy has no such key, so a resend that reaches the backend runs the job again. This is a real difference between the two designs, and the results say so rather than hiding it.

Nobody gives up: as in chains, work is retried until it succeeds, so every run does all its work and we compare cost and time.

## Traffic

- **Tenants:** 3 large (3 requests/s at 1×) and 12 small (1 each), spread over `ord`, `lhr` and `syd`. 1× is 21 requests/s, 42 backend jobs/s, about the backend's starting capacity (40/s). Two client machines per region, the large tenant on its own.
- **Shape** (40 min): 5 min at 0.5×, 10 at 1×, a peak at 3× from 15 to 25 min, 10 at 1.5×, 5 at 0.5×. Seven bursts of at least 4×, 10–60 s long, at fixed times outside the peak, so every run gets the same traffic.
- **Outage** (one, decided): the backend refuses everything for 60 s at 20 min, in the middle of the peak, then comes back. This is when no-jitter clients retry in waves.
- **Capacity:** doubles at 25 min, as in chains.

## Front doors

Two setups. The Envoy configuration used since `env` is already the careful one (per-tenant and global rate limits with `Retry-After`, adaptive concurrency, admission control, circuit breakers, a retry budget, and two retries of its own), so a separate tuned variant would be the same file; the plan for three setups was dropped.

1. **Envoy**, configured as in every earlier series.
2. **Aquifer**, at the same commit and with the same live headers from the backend as in `env2` and chains.

Fairness, both ways:

- The services, clients, timeouts and retry policies are identical in both setups; only the bottom call differs (`/work` through Envoy, or `/proxy` through Aquifer).
- The bottom services keep their 5 s timeout with Aquifer too, although a job held in a queue can take longer than that. A team adopting Aquifer would likely wait longer or take a webhook; keeping 5 s is the stricter choice for Aquifer, and every re-wait it causes is counted as a request.
- Requests to the front door are counted for both. Under Aquifer a repeat costs a request but no backend work; the results report both numbers, not only backend executions.
- The backend's outage refuses Aquifer's dispatches with a 503 the same way it refuses Envoy's; its live headers don't announce the outage.

## What is measured

| Claim | Measure |
|---|---|
| Cheaper | backend requests per client request (the amplification); requests per completed job at each hop; peak request rate |
| Faster | time to finish the same amount of work; time from the outage ending until the backlog is cleared; p50 and p99 per behavior |
| More reliable | duplicate executions; work done for a caller that had already timed out; backend errors |
| Fair | share of the backend per tenant and per behavior; whether the aggressive and no-jitter instances take more than their share |

## Cost

- Smoke test, one round with short phases: about $0.50.
- Full series, 3 rounds of the two setups (6 runs): about $4–6, a little more than chains because each run adds four small service machines.

## Decided

- Client timeout: 15 s.
- One 60 s outage.
- 15 tenants (3 large, 12 small), 10 workers each (40 for large), 240 workers in all.
