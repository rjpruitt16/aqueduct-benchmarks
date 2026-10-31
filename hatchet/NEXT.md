# Next Hatchet experiments: design for review

Status: experiments 1–3, an outage test and a smaller-database test signed off together on 2026-10-28 and running (`deploy/fly/next.sh`); experiment 4 isn't built.

The pilot and speed series showed that Aquifer, paced at or below Hatchet's limit, bounds Hatchet's internal backlog, cuts Postgres reads 5–10×, and keeps small tenants fast and unrefused. It did not reduce Postgres CPU, and every comparison so far was against Hatchet accepting everything, which never makes callers retry. These experiments close those gaps. All of them stay fair to the alternative: Hatchet and its database near most users, every alternative tuned as a careful team would, every request counted.

## 1. Patch 2: Hatchet reports its load

**Status: done.** Built in the fork (`237971d`) and run three times; see [PILOT.md](PILOT.md#patch-2-hatchet-tells-aquifer-how-fast-to-send). The rate is for the whole tenant, divided by the `X-Aqueduct-Active-Queues` the caller sends. Patch 3 (`503` while Postgres is unreachable) is not built.

**Why.** Stock Hatchet never tells a caller it is overloaded: it accepts the work and slows down. Aquifer then has nothing to adapt to and behaves like a static limit, so it can't follow a database that grows.

**What.** In the fork (`rjpruitt16/hatchet-aquifer`), the REST trigger response carries:

- `X-Aqueduct-Max-Backlog`: how much more work Hatchet wants queued for it.
- `X-Aqueduct-Rps`: the trigger rate Hatchet can sustain now.

Both are derived from a cheap, already-available signal, sampled at most once a second:

- Hatchet's own queue depth (the `v1_concurrency_slot` count the scheduler already keeps), against a target depth. Above the target, the advertised rate falls; below it, it rises.
- Optionally the engine's trigger write slots (`SERVER_..._TRIGGER_SLOTS`) in use, which Patch 1 already reads.

Default off behind an env var, like Patch 1. Also check what the API answers while Postgres is unreachable; if it is `500`, a small Patch 3 makes it `503` with `Retry-After`, so callers and Aquifer treat it as temporary.

**Open question for you:** whether the advertised rate should be per tenant or for the whole API. Aquifer applies `X-Aqueduct-Rps` per account queue today, while the backlog budget is per target API.

## 2. D: Hatchet behind Envoy with client retries

**Status: built and running** (`CONFIG=D` in `run.sh`, `next.sh baseline`): three D runs, two C runs (Patch 2) and one B run. Every producer retries until its document is accepted, and Aquifer retries each job until Hatchet takes it, because callers in the wild don't give up. Envoy allows each tenant 15 triggers/s under a total of 20/s (Hatchet's calibrated capacity); the driver's status polls go straight to Hatchet, so they don't use up Envoy's limits.

The configuration most teams run today. Envoy in front of Hatchet's API, tuned the same way as in the [Envoy benchmark](../envoy/README.md): per-tenant rate limits, admission control, adaptive concurrency, circuit breakers with a pending queue, retry budget. The driver retries refused triggers like an SDK (full-jitter backoff, `Retry-After`).

Measures, beyond the pilot's: requests per completed document, refused requests per second, Hatchet API CPU, and per-tenant share. Three runs at the calibrated rate, against three C runs at 80%. About $5.

**Postgres CPU, and what it does and doesn't show.** Every run now records each machine's CPU from Fly's metrics (`cpu.json`), and the pilot runs were backfilled the same way (`ana/backfill-cpu.sh`). In the pilot, Postgres ran at 1.3–1.9 of its 2 cores with or without Aquifer, and did about the same useful work per CPU-second: 8.6–11.6 documents per Postgres CPU-second for B, 8.7–11.2 for C. Hatchet on its own never refuses a trigger, so no caller retries and there is no retry storm for Aquifer to remove. D, where Envoy refuses and callers retry, is where wasted CPU would show.

## 3. Postgres resize mid-run

**Status: built and running** (`RESIZE_AT`, `RESIZE_TO` in `run.sh`, `next.sh resize`), two runs each of B, D and C.

Postgres starts at `performance-2x`. Ten minutes into the sustained overload it is resized to `performance-4x` (a machine update: Postgres restarts on the same volume). Measured for each of B, D and C (C with Patch 2):

- documents lost, failed or run twice;
- errors callers saw during the restart, and for how long;
- time until throughput uses the larger machine;
- small tenants' latency through the restart.

Without Patch 2, C can't use the new capacity, so this runs after Patch 2. Two runs each, about $8.

## 3b. Postgres outage

**Status: built and running** (`OUTAGE_AT`, `OUTAGE_FOR` in `run.sh`, `next.sh outage`). Postgres is stopped ten minutes into the sustained overload, for 5 minutes and for 15, once each for B, D and C. It answers how long an outage each setup rides out with nothing lost: Aquifer holds what it has accepted (up to its backlog budget and disk), while Hatchet's API is down for everyone else's callers.

**Results, and a rerun with a bigger backlog budget (2026-10-30).** In the first outage runs (`nx-out5-c`, `nx-out15-c`), C did worse than D after the outage: small tenants' recovery p99 was 107 s against D's 3.8 s. The cause was the backlog budget the Hatchet fork advertises, `X-Aqueduct-Max-Backlog`, sized as 60 seconds of its rate (1,200 jobs at 20 rps). The outage filled it at once, Aquifer refused every tenant with `upstream_queue`, and the driver's own backoff stretched each refusal to about 100 s. The rerun used `AQUEDUCT_BACKLOG_SECONDS=600` and Aquifer with aquifer#135 (a shrinking budget no longer locks small tenants out); the run can't separate the two changes.

| Run | Outage | Small tenants' recovery p99 | Small tenants' attempts per document | Failed |
|---|---|---|---|---|
| `nx-out5-c` (60 s budget) | 5 min | 111 s | 1.38 | 23 |
| `nx-bl-out5-c-1` (600 s) | 5 min | 5.7 s | 1.00 | 0 |
| `nx-bl-out5-c-2` (600 s) | 5 min | 3.4 s | 1.00 | 0 |
| `nx-out5-d` (Envoy) | 5 min | 3.9 s | 1.36 | 1 |
| `nx-out15-c` (60 s) | 15 min | 403 s | 2.04 | 1 |
| `nx-bl-out15-c` (600 s) | 15 min | 385 s | 1.20 | 20 |
| `nx-out15-d-2` (Envoy) | 15 min | 395 s | 1.90 | 0 |

With the 600 s budget and a 5-minute outage, small tenants and accounting were never refused; every refusal went to law-firm, the tenant flooding the system. A 15-minute outage is longer than the 600 s budget, so Aquifer filled to its hard ceiling (15,000, 1.25x the budget) and refused everyone, as the sizing rule predicts; small tenants then recovered about as fast as under Envoy, with far fewer attempts. The no-outage baseline with the bigger budget (`nx-bl-c-1`) matches the earlier C baselines. A second baseline (`nx-bl-c-2`) failed to launch (Fly 408) and wasn't rerun.

Not explained yet: C's outage runs had Hatchet runs end `FAILED` (23 in `nx-out5-c`, 20 in `nx-bl-out15-c`, 0 in the 5-minute reruns), against 0-1 for D; and one duplicate completion in `nx-bl-out5-c-1`, while D's `nx-out15-d-2` had 13. Only `summary.json` and `run.txt` are committed for these runs.

## 3c. A smaller database

**Status: built and running** (`PG_SIZE` in `run.sh`, `next.sh smalldb`). C twice and B once with Postgres on `performance-1x`, half the CPU, against the earlier runs on `performance-2x`. It answers the cost question directly: can Hatchet behind Aquifer do the same work on a smaller database?

## 4. Multi-region callers

Hatchet and Postgres stay in one region, next to most of their users. Tenants submit from several Fly regions, mostly nearby (`iad`, `ord`, `dfw`) with one farther away (`lhr`), so the big tenant isn't the only one close to the database. Run D and C (Patch 2) for an hour each. This shows whether being close to the database, or retrying faster, lets a tenant take more than its share under Envoy, and what the round trips of retries cost far-away tenants. The driver splits into one producer per region, reporting to a central ledger. About $6.

## Order and total

Patch 2 (code only, no spend) → D → resize → multi-region. Roughly $20 of Fly time in total, asked for one experiment at a time.

As run (2026-10-28/29): a smoke test, then baseline (6 runs), resize (6), outage (6) and smaller database (3), about $25 of Fly time. Multi-region isn't built.
