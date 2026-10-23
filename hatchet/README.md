# Hatchet

[Hatchet](https://github.com/hatchet-dev/hatchet) is a durable task orchestrator built on Postgres. This benchmark asks: when bursts arrive faster than Hatchet's Postgres can absorb them, and while that database is scaled up or out, does Aquifer in front of Hatchet keep Postgres healthy, get more work done, and protect small tenants, compared with Hatchet's own controls properly tuned?

**Status:** harness under construction. No results yet.

## Pinned versions

- Hatchet engine v0.110.8 (full self-hosted for measured runs; Hatchet Lite for local development)
- Python SDK 1.43.0 for the issue reproductions

## Workload

A multi-tenant document pipeline: parse, extract, embed and index, four tasks per document, with optional per-page child runs. Producers represent a law firm sending large batches, an accounting firm sending medium batches, and several small businesses sending a trickle. Producers submit through Hatchet's REST API and poll document status the way real applications do.

In the first experiment the four steps are fixed-duration simulations, so Postgres sees the pipeline's real write pattern without PDF processing getting in the way.

## Configurations

| | |
|---|---|
| **A** | Stock Hatchet defaults |
| **B** | Hatchet tuned with its own controls (per-tenant round-robin concurrency, priorities, scheduling timeout, rate limits) |
| **C0** | B with Aquifer in front, using a fixed rate cap |
| **C** | B with Aquifer in front of a Hatchet build that reports its own pressure |

## Phases of each run

Steady load, a burst at three times capacity, sustained overload, and recovery. Variants add per-page fan-out, a Postgres resize mid-overload, and adding a read replica mid-burst.

## Measurements

- **Speed:** completed documents per minute, p50/p95/p99 time from first offer to completion, time to drain after the burst.
- **Fairness:** each tenant's latency during the burst relative to its own steady state, Jain's index on demand-normalized throughput, and the longest gap in which a small tenant completed nothing.
- **Postgres:** CPU, IOPS, connections, lock waits, query latency, read/write split, replica lag.
- **Correctness:** lost jobs, duplicate runs, and jobs that timed out before starting, per 10,000 offered.

## Reported issues checked on the pinned version

| Issue | Result on v0.110.8 |
|---|---|
| [#4287](https://github.com/hatchet-dev/hatchet/issues/4287) Scheduling timeouts look like failures | Reproduces: runs that time out in the queue end FAILED with no start time and no error message |
| [#3922](https://github.com/hatchet-dev/hatchet/issues/3922) Parent shows success when children time out | Reproduces: `aio_run_many` returns `{}` for timed-out children and the parent completes |

The reproduction is in [`repro/`](repro/).

## Harness

- [`cmd/worker`](cmd/worker): runs the four-step document workflow. `CONFIG=A` uses Hatchet's defaults; `CONFIG=B` adds per-tenant round-robin concurrency (`input.tenant`) and a long scheduling timeout. The index step reports each completion to the driver.
- [`cmd/driver`](cmd/driver): the producers and the ledger. It offers documents for seven tenants on the phased schedule (`PHASES`, scaled by `SCALE`), submits them through Hatchet's REST trigger with exponential backoff that honors `Retry-After`, polls run status every few seconds like an application would, and classifies every document as completed, failed, never started (#4287) or rejected. It writes `intervals.jsonl` every 10 seconds and `summary.json` at the end: per-tenant outcomes, p50/p99 by phase, slowdown during overload, longest gap without a completion, Jain's index, and drain time.

- [`cmd/pgsampler`](cmd/pgsampler): every 10 seconds, one line of Postgres load (connections by state, waiting sessions by type, lock waits, commits and rows per second, cache hit ratio, database size, and the size and live rows of Hatchet's busiest tables). On exit it writes the 30 statements with the most execution time from `pg_stat_statements`.

### Submitting through Aquifer (C0 and C)

With `AQUIFER_URL` set, the driver posts each document to Aquifer's `/jobs` (`user_id` is the tenant, `idempotent_key` the document) aimed at Hatchet's REST trigger, and receives Hatchet's response through Aquifer's webhook at `PUBLIC_URL/aquifer`. Aquifer's `201` counts as acceptance. With Aquifer's default 100 ms flush, a job reaches disk within 100 ms of the `201`, so a crash in that window could lose it; set `AQUIFER_FLUSH_MS=0` in the Fly run to make every `201` wait for its own fsync, at lower throughput. The runs use Aquifer's default unless stated. A job Aquifer gives up on is counted as `gave_up`, separately from `rejected`, because the producer was told it was accepted. `to_run_id_p99_ms` in the summary is the time from first offer until the driver learned the document's Hatchet run ID: the producer's own retries without Aquifer, or, with it, the wait in Aquifer plus the webhook back.

## Fork: hatchet-aquifer

[rjpruitt16/hatchet-aquifer](https://github.com/rjpruitt16/hatchet-aquifer) (branch `aqueduct/v0.110.8`, see its `AQUEDUCT.md`) is an unofficial fork carrying the Aqueduct patches. Images: `ghcr.io/rjpruitt16/hatchet-aquifer-api:v0.110.8-aqueduct-<commit>`.

1. **429 for ResourceExhausted on the REST trigger.** Checked against a running Hatchet with `SERVER_ENFORCE_LIMITS=t` and a task-run limit of 8: stock v0.110.8 answers the triggers past the limit with **400**, the fork with **429**.

## What we learned about Hatchet's own backpressure

- **Bursts are not rejected.** The engine allows `SERVER_GRPC_TRIGGER_WRITE_SLOTS` (default 5) concurrent fast-path trigger writes. Past that, an ordinary trigger falls back to Hatchet's message queue, which is Postgres in this setup, instead of failing. `ResourceExhausted` only comes from enforced tenant limits, or from an idempotent trigger that can't get a slot. So Hatchet accepts a burst and carries it as extra database writes; the trigger-slot setting is part of B's tuning.
- **Tenant limits are off for self-hosters by default** (`SERVER_ENFORCE_LIMITS=false`). If enabled, the defaults are small (2,000 task runs per 24 hours), so they must stay off or be raised for the benchmark.

### Fly runs (script written, not yet run)

[`deploy/fly/run.sh`](deploy/fly/run.sh) runs one measured configuration (`B` or `C`) in a private Fly app and destroys it afterward. The harness image, with the worker, driver, Postgres sampler and Aquifer from a pinned commit, is built by `.github/workflows/hatchet-image.yml` as `ghcr.io/rjpruitt16/aqueduct-benchmarks-hatchet:<commit>`.

| Machine | Size | Role |
|---|---|---|
| `pg` | performance-2x, 4 GB, 20 GB volume | Postgres 16, also Hatchet's message queue |
| `engine` | performance-2x, 4 GB | Hatchet engine |
| `api` | performance-1x, 2 GB in C; performance-2x, 4 GB in B | Hatchet REST API (the fork's image in C). B gets Aquifer's machine budget back here |
| `worker-1..3` | performance-1x, 2 GB | pipeline workers |
| `aquifer` | performance-1x, 2 GB, 5 GB volume | C only; its job store is on the volume |
| `driver` | performance-2x, 4 GB, 1 GB volume | producers, ledger, Postgres sampler |

Results land in `results/<run-name>/`: `summary.json`, `intervals.jsonl`, `postgres.jsonl`, `statements.json`, `driver.log`, `machines.json` and `run.txt` (versions, images, rate).

On Fly the engine must listen on IPv6 (`SERVER_GRPC_BIND_ADDRESS=[::]`), because the private network is IPv6-only; checked locally that workers connect to an engine bound that way.

### Local full self-hosted Hatchet

[`deploy/local`](deploy/local) runs the same shape as the Fly runs: Postgres, migrations, seed, engine and API as separate containers, configured only by environment variables (no shared config volume), with Postgres as the message queue and `pg_stat_statements` loaded.

```sh
deploy/local/setup.sh    # generates keys, starts Hatchet, writes deploy/local/token.txt
export HATCHET_CLIENT_TOKEN=$(cat deploy/local/token.txt) HATCHET_CLIENT_TLS_STRATEGY=none HATCHET_CLIENT_HOST_PORT=localhost:7077
CONFIG=B STEP_MS=20 CALLBACK_URL=http://localhost:8090/done go run ./cmd/worker &
HATCHET_API=http://localhost:8080 LISTEN=:8090 RATE=20 SCALE=0.02 go run ./cmd/driver
(cd deploy/local && docker compose down -v)
```

Two checks on this setup (2026-10-23, scaled schedule, about 1.5 minutes):

| Run | Result |
|---|---|
| `CONFIG=B`, 20 docs/s | 1,917 offered, all completed, no duplicates, nothing outstanding |
| `CONFIG=B`, one run per tenant, 2 s scheduling timeout, 100 ms steps | 292 completed, 664 never started (656 law firm, 8 accounting); every small business completed all of its documents. Jain's index 0.86 |

The second run is a classification check, not a benchmark result: it shows the ledger finds never-started timeouts and that per-tenant round-robin protects small tenants.
