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
