# Design: the wild regime

Status: draft, not built. Nothing here runs until it is signed off.

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
- **Cascading retries.** Every service retries the one below it 3 times quickly (50, 100, 200 ms, no jitter), then returns 503 to its caller, which retries in turn. Worst case per backend job, per client attempt: 3 (`api`) × 3 (`orders`) × 3 (`inventory`/`payments`) = 27 requests at the front door, before the client's own retries. This is the retry storm that takes down real systems, and each layer's retries are reasonable on their own.
- **Timeouts at every hop.** Each service gives up on the one below after a timeout (`api` 10 s, `orders` 8 s, leaves 5 s) and retries, while the abandoned call may still be running underneath. That is where abandoned and duplicate work comes from.
- **The front door sits only in front of the backend**, the one shared resource that runs out, which is where a team would put Envoy or Aquifer. The services between are plain HTTP in both setups, so the only difference is what happens at the bottom:
  - Envoy mode: the backend's overload comes back as 429s and 503s, and every layer above multiplies them.
  - Aquifer mode: `inventory` and `payments` submit their job to Aquifer and wait for the result (`/proxy`). While Aquifer has room nothing is refused, so nothing above retries. When its backlog is full it refuses at submit, and the same retry layers apply.
- Each service counts the requests it receives and makes, so the amplification can be read at every hop, not only at the bottom.

## Client behavior

Each tenant runs 10 concurrent workers (large tenants 40), each with its own retry state, so retries come from many independent sources, not one tidy loop per tenant. A worker sends one request at a time and waits until it succeeds before sending the next. Behaviors are assigned per tenant, so a tenant's workers all behave alike and tenants can be compared by behavior.

| Behavior | Share of tenants | What it does |
|---|---|---|
| Polite | 40% | exponential backoff with full jitter, honors `Retry-After` |
| No jitter | 30% | exponential backoff without jitter, so workers that fail together retry together, in waves |
| Aggressive | 20% | retries every 100 ms, ignores `Retry-After` |
| Library stack | 10% | an HTTP library (3 quick tries) inside an SDK (3 tries) inside the app's loop |

All of them also have a **timeout**: after 15 s (decided) without an answer they abandon the request and send it again, while the first copy may still be running. In Aquifer mode the resend reuses the job's idempotency key, as Aquifer's API asks clients to do; in Envoy mode there is no such key, so the backend runs it twice. This is a real difference between the two designs, and the results say so rather than hiding it.

Nobody gives up: as in chains, work is retried until it succeeds, so every run does all its work and we compare cost and time.

## Traffic

- **Tenants:** 3 large and 12 small, spread over `ord`, `lhr` and `syd`, with each behavior mixed across regions.
- **Shape** (40 min): a slow rise to 1×, random bursts of 4× lasting 10–60 s, a peak at 3× from 15 to 25 min, then a fall to 0.5×. The burst times are drawn from a fixed seed, so every run gets the same traffic.
- **Outage** (one, decided): the backend refuses everything for 60 s at 20 min, in the middle of the peak, then comes back. This is when no-jitter clients retry in waves.
- **Capacity:** doubles at 25 min, as in chains.

## Front doors

Three setups, so Envoy is not a straw man:

1. **Envoy, default:** the per-tenant and global rate limits used so far.
2. **Envoy, tuned:** what a careful operator would add: retry budget, circuit breakers, outlier detection, `healthy_panic_threshold: 0`, and request timeouts matching the clients'.
3. **Aquifer:** live headers from the backend, as in `env2` and chains.

## What is measured

| Claim | Measure |
|---|---|
| Cheaper | backend requests per client request (the amplification); requests per completed job at each hop; peak request rate |
| Faster | time to finish the same amount of work; time from the outage ending until the backlog is cleared; p50 and p99 per behavior |
| More reliable | duplicate executions; work done for a caller that had already timed out; backend errors |
| Fair | share of the backend per tenant and per behavior; whether the aggressive and no-jitter instances take more than their share |

## Cost

- Smoke test, one round with short phases: about $0.50.
- Full series, 3 rounds of the three setups (9 runs): about $6–9, a little more than chains because each run adds four small service machines.
- Before any run: a fresh Fly token on the Sprite (the last one expired mid-series).

## Decided

- Client timeout: 15 s.
- One 60 s outage.
- 15 tenants (3 large, 12 small), 10 workers each (40 for large), 240 workers in all.
