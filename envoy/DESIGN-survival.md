# Design: how much time does it buy?

Status: signed off 2026-10-27, built.

## The question

Traffic jumps to three times what the backend can handle, and new capacity arrives some minutes later, when an autoscaler or a person catches up. How much of the work still gets done, and how long can the new capacity take before work starts being lost? That delay is the time the front door buys.

## Why this shape

An earlier draft had a backend that crashes under load. Our Envoy already has circuit breakers, adaptive concurrency and admission control, and Aquifer follows the backend's limits, so neither would let it crash: "time to first crash" would read "never" for both. The difference between them is where the waiting work sits:

- **Envoy** refuses what the backend can't take, and the callers hold the work: they retry until capacity arrives, for as long as their patience lasts.
- **Aquifer** holds the work itself and runs it when capacity arrives, whether or not the caller is still waiting. What bounds it is the backlog the backend allows (`X-Aqueduct-Max-Backlog`) and Aquifer's own memory and disk.

## Setup

- **Workload:** independent jobs, one hop: client → front door → backend. No chain of services, so no stacked retries.
- **Callers:** the same loop for every front door, Envoy's best case: up to 10 s per attempt, exponential backoff with full jitter capped at 60 s, honoring `Retry-After`, and **giving up 5 minutes after the job was created**. Envoy callers send to `/work`; Aquifer callers to `/proxy`, with the job's ID as the idempotency key, so a retry waits on the job Aquifer already holds.
- **Tenants** (jobs/s at 1×; 1× = 40 jobs/s, the starting capacity): big 20 (`ord`), medium 10 and small-1 2.5 (`iad`), small-2 and small-3 2.5 (`dfw`), small-4 2.5 (`lhr`). small-4 is aggressive: it retries every 100 ms and ignores `Retry-After`, reported separately as the one bad actor.
- **Load:** 0.5× for 3 minutes, 3× for 15, 0.5× for 12.
- **Capacity:** 8 workers at 200 ms (40 jobs/s), going to 32 (160 jobs/s) **D minutes into the 3×**, for D = 1, 3, 5 and 10.
- **The backend** advertises its totals (`X-Aqueduct-Max-Concurrent`, `X-Aqueduct-Rps`), with `X-Aqueduct-Max-Backlog` at 30 minutes of its capacity, and refuses with a 503 past 64 waiting, as in every series.

## Front doors

| Setup | Limits |
|---|---|
| `envoy` | The config from every series, with limits a team would set for today's capacity: 20/s per tenant, 80/s in total |
| `envoy-max` | The same config with the limits in the same proportions to the capacity after scaling: 80/s per tenant, 320/s in total |
| `aquifer` | v0.16.0, following the backend's headers under a ceiling of 320/s |

Envoy's rate limits and circuit-breaker limits are fixed in its config: they don't grow when capacity does. Adaptive concurrency (up to 64 in flight), admission control and load balancing do adapt. `envoy` shows what fixed limits sized for today cost when capacity grows; `envoy-max` gives Envoy limits sized for later, which let the overload through before then.

Variants, Aquifer only (Envoy has no headers or keys to take away), at D = 5:

- **No headers** (`ADVERTISE=none`): the backend says nothing, and Aquifer runs on limits a team would set for today, like `envoy`: 80/s in total, 2 in flight per tenant.
- **No key** (`NO_KEY=1`): callers send a new idempotency key on every attempt, as a caller with no stable job ID would. The backend still sees the real job ID, so repeats are counted.

## What is measured

Two numbers side by side for every run, because they answer different questions:

1. **Answered in time:** the caller got a successful answer before giving up. This is what a waiting person or a synchronous API call needs; holding work doesn't help it.
2. **Ran at the backend:** the job ran at least once, by the backend's count of unique job IDs, even if its caller had given up. With Aquifer the result is kept for the caller to look up later; with Envoy a run whose caller gave up is a side effect nobody heard about.

Also: requests per job, repeats of work already done, latency (p50, p99) during the overload, the aggressive tenant against the polite ones, and how busy the backend was after capacity arrived.

**Time bought** is the largest D at which (1) and (2) stay near 100%.

## Fairness

- The same caller loop, patience and backoff for every front door; only the endpoint differs.
- Envoy keeps the config from every series. Its fixed limits are reported as a cost, not tuned away: `envoy-max` is there so the best-case Envoy is shown too.
- Both numbers are reported for every setup. If Aquifer only wins on (2), the write-up says so: it buys time for work that can finish later (webhooks, jobs, agent tasks), not for a caller who needs an answer now.

## Runs and cost

| Step | Runs | Estimate |
|---|---|---|
| Smoke: `envoy`, `envoy-max`, `aquifer`, 8 minutes | 3 | about $1.50 |
| Series: 3 setups × D = 1, 3, 5, 10 × 2 rounds | 24 | about $12 |
| Variants: no headers, no key, at D = 5 × 2 rounds | 4 | about $2 |
| Total | 31 | about $15 |

A run is 6 machines for about 40 minutes. `deploy/survival-series.sh` runs one round of two delays at a time (6 apps).

## Left for later

- A backend that crashes under load, if a later setup can let it.
- The front door's own breaking point (Aquifer on a small machine, Redis for a do-it-yourself queue), once these results are in.
- Capacity as new machines rather than more workers in one, after the scale-out fixes.
