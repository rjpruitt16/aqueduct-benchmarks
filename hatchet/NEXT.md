# Next Hatchet experiments: design for review

Status: proposal, nothing built. Each experiment needs sign-off before code or spend.

The pilot and speed series showed that Aquifer, paced at or below Hatchet's limit, bounds Hatchet's internal backlog, cuts Postgres reads 5–10×, and keeps small tenants fast and unrefused. It did not reduce Postgres CPU, and every comparison so far was against Hatchet accepting everything, which never makes callers retry. These experiments close those gaps. All of them stay fair to the alternative: Hatchet and its database near most users, every alternative tuned as a careful team would, every request counted.

## 1. Patch 2: Hatchet reports its load

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

The configuration most teams run today. Envoy in front of Hatchet's API, tuned the same way as in the [Envoy benchmark](../envoy/README.md): per-tenant rate limits, admission control, adaptive concurrency, circuit breakers with a pending queue, retry budget. The driver retries refused triggers like an SDK (full-jitter backoff, `Retry-After`).

Measures, beyond the pilot's: requests per completed document, refused requests per second, Hatchet API CPU, and per-tenant share. Three runs at the calibrated rate, against three C runs at 80%. About $5.

## 3. Postgres resize mid-run

Postgres starts at `performance-2x`. Ten minutes into the sustained overload it is resized to `performance-4x` (a machine update: Postgres restarts on the same volume). Measured for each of B, D and C (C with Patch 2):

- documents lost, failed or run twice;
- errors callers saw during the restart, and for how long;
- time until throughput uses the larger machine;
- small tenants' latency through the restart.

Without Patch 2, C can't use the new capacity, so this runs after Patch 2. Two runs each, about $8.

## 4. Multi-region callers

Hatchet and Postgres stay in one region, next to most of their users. Tenants submit from several Fly regions, mostly nearby (`iad`, `ord`, `dfw`) with one farther away (`lhr`), so the big tenant isn't the only one close to the database. Run D and C (Patch 2) for an hour each. This shows whether being close to the database, or retrying faster, lets a tenant take more than its share under Envoy, and what the round trips of retries cost far-away tenants. The driver splits into one producer per region, reporting to a central ledger. About $6.

## Order and total

Patch 2 (code only, no spend) → D → resize → multi-region. Roughly $20 of Fly time in total, asked for one experiment at a time.
