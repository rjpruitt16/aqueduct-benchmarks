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
