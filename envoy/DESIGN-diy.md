# Design: Envoy plus what a team would build itself

Status: signed off and built.

## Why

In the wild regime the same backend completed about 2.2 times as many requests behind Aquifer as behind Envoy, because under Envoy 55–58% of the backend's work repeated jobs already done. A fair objection is that Envoy was compared as a bare proxy. A team that hit this would add idempotency keys in its services, and maybe a queue, and keep Envoy. This regime builds those, in the ordinary way a team would, and runs them under the same wild traffic.

It answers two questions:

1. How much of Aquifer's result comes from idempotency alone, and how much from holding the work in a fair queue?
2. What does it take to get there without Aquifer: extra machines, extra calls to a shared store, and code to own?

The hypothesis: idempotency alone removes most duplicate backend work but not the retry storm, because a refusal at the bottom is still multiplied on the way up. Idempotency plus a fair queue should come close to Aquifer, at the cost of a Redis instance, a dispatcher, and the code that holds them together. The runs may say otherwise.

## Setups

The wild regime unchanged (same services, clients, tenants, retry styles, traffic, bursts, outage and capacity change), with four front doors run side by side in each round:

| Setup | Bottom services call | Shared store |
|---|---|---|
| `envoy` | Envoy `/work`, as in wild | none |
| `envoy-idem` | Envoy `/work`, behind an idempotency check | Redis |
| `envoy-queue` | a per-tenant queue in Redis; a dispatcher sends to the backend through Envoy | Redis |
| `aquifer` | Aquifer `/proxy`, as in wild | none (Aquifer's own) |

Envoy keeps the configuration used in every series, in all three Envoy setups.

### `envoy-idem`: idempotency keys

The Stripe-style pattern, in `inventory` and `payments`, keyed by the job ID every resend already carries:

1. `GET result:<job>`. If it is there, answer from it; no backend call.
2. Otherwise `SET lock:<job> NX PX 30000`. If the lock is taken, another copy of this job is running: wait for its result (`BLPOP done:<job>` until this try's timeout), then answer as in 1, or fail the try if it times out.
3. If this copy holds the lock, call Envoy `/work`. On success, store `result:<job>` (kept for an hour) and push to `done:<job>` to wake waiters. On a refusal or error, release the lock so the next try can go.

There is no queue: a job the backend refuses is still refused, and every layer above retries as before. This isolates what deduplication does on its own.

### `envoy-queue`: idempotency plus a fair, durable queue

The pattern a team builds when it wants work held instead of refused:

1. `inventory` and `payments` check `result:<job>` as above. If the job is not done and not already queued (`SET queued:<job> NX`), they push it onto the tenant's list (`RPUSH q:<tenant> <job>`) and add the tenant to the active set.
2. Every caller of that job, first or repeat, then waits for the result (`BLPOP done:<job>` until its try's timeout). A timed-out try fails as it would against Aquifer and is retried from above, but the job stays queued and is not queued again.
3. A **dispatcher** (new, one machine in `ord`) takes jobs round-robin across tenants with a non-empty list and sends each to the backend through Envoy. It keeps as many in flight as the backend advertises in `X-Aqueduct-Max-Concurrent`, the same live header Aquifer follows, and waits out `Retry-After` on a refusal before trying that job again. On success it stores the result and wakes the waiters.

Redis runs with append-only persistence, so the queue survives a restart; nothing restarts during a run, but a team would want that.

## Fairness

- **To the do-it-yourself setups.** They get the same information Aquifer gets: the job ID as an idempotency key, and the backend's live concurrency header. Without the header the dispatcher would need a fixed limit, which would be wrong half the run because capacity doubles at 25 minutes. Waiters use a blocking pop, not polling, so waiting costs no extra calls.
- **To Aquifer.** The extra machines (Redis, dispatcher) and every Redis call are counted as cost. Aquifer is a single machine with its own store, and the results say so.
- **The same timeouts everywhere.** The bottom services keep their 5 s timeout in all four setups, so a job held in a queue causes re-waits in `envoy-queue` the same way it does in Aquifer.
- **Code to own** is reported for each setup: the lines of service and dispatcher code added beyond the wild regime, and what they depend on.
- **Same Envoy image and config** in all three Envoy setups; same Aquifer commit as wild (`630501d`).

## What is measured

Everything the wild regime measures, plus:

| Measure | Why |
|---|---|
| Redis calls per completed request | the shared-store cost of doing it yourself |
| Redis and dispatcher CPU and memory | the size of the extra machines |
| Jobs waiting in the queue over time, per tenant | whether the queue is fair and how fast it drains after the outage |
| Lines of code added, dependencies | what a team would build and keep |

## Build

- `cmd/service`: `MODE` gains `envoy-idem` and `envoy-queue`; `REDIS_URL` added. Redis through `github.com/redis/go-redis/v9`, the client most Go teams use.
- `cmd/dispatcher` (new): the round-robin dispatcher, reporting its counts every 10 s like the services.
- `deploy/run.sh`: the two new modes; a Redis machine (`redis:7`, append-only on, shared-cpu-2x, 1 GB) and, for `envoy-queue`, the dispatcher; Redis `INFO` stats saved at the end of a run.
- Local check on the Mac first, tiny, as for wild; then one 5-minute smoke run per new setup on Fly; then the series.

## Cost

| Step | Estimate |
|---|---|
| Smoke runs (two new setups) | about $0.50 |
| Series: 3 rounds of the two new setups, plus one round of `envoy` and `aquifer` as a check | about $7–9 |

## Decided

1. **Runs.** The two new setups run for three rounds. Round 1 also runs `envoy` and `aquifer` side by side with them, as a check against the wild series. If the check matches wild, the new setups are compared with all of wild's rounds; if it doesn't, that is reported first. The wild rounds agreed closely (Aquifer 46,200–46,300 completed, Envoy 20,500–22,200), so day-to-day noise is far smaller than the differences being measured.
2. **A repeat of a running job in `envoy-idem` waits** for the running copy's result, rather than getting a 409 and retrying.
3. **One dispatcher.**
