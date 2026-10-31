# Configuration B: tuned Hatchet

Configuration B is Hatchet with its own controls set the way a careful team would set them for this workload. It is the baseline Aquifer has to beat, so every setting here is taken from Hatchet's documentation (*Improving Performance*, *Concurrency*, *Timeouts*, *Rate Limits*) or from reading the v0.110.8 source, and the same settings are used in C. A uses Hatchet's defaults throughout.

Values marked *calibrated* are fixed during the calibration run and then held constant for every measured run. This file records the chosen values.

## Workflow

| Setting | A | B and C | Why |
|---|---|---|---|
| Per-tenant concurrency | none | `GROUP_ROUND_ROBIN` on `input.tenant`, `max_runs` 50 | Hatchet's native fairness: a tenant's documents queue behind its own, and tenants take turns |
| `schedule_timeout` | 5 min (default) | 1 h | A burst should wait, not fail before starting (#4287). One hour covers the longest backlog the schedule can build |
| Worker slots | 200 per worker | 200 per worker | Workers are sized so they are never the bottleneck in experiment 1 |

## Engine

| Setting | Default | B and C | Why |
|---|---|---|---|
| `DATABASE_MAX_CONNS` | 50 | 100 | The guide's first lever for high throughput. Postgres `max_connections` is 400, so engine and API together stay below it |
| `SERVER_GRPC_TRIGGER_WRITE_SLOTS` | 5 | 20 (calibrated) | Concurrent fast-path trigger writes. Past this, triggers fall back to the message queue (more database writes). Found in the source; not in the performance guide |
| `SERVER_DEFAULT_BUFFER_SIZE` | 10 | 50 (calibrated) | Larger buffers batch more message-queue writes per round-trip, lowering database load for a little latency |
| `SERVER_DEFAULT_BUFFER_FLUSH_INTERVAL` | 10 ms | 25 ms (calibrated) | Same trade-off as the buffer size |
| `SERVER_ENFORCE_LIMITS` | false | false | Tenant limits stay off. Enabled, the defaults (2,000 task runs per 24 h) would reject the benchmark itself |

`SERVER_MSGQUEUE_RABBITMQ_QOS` does not apply: Postgres is the message queue.

## Postgres

Postgres 16 on a dedicated performance-2x machine (2 CPUs, 4 GB):

```
max_connections=400
shared_buffers=1GB
effective_cache_size=3GB
work_mem=16MB
shared_preload_libraries=pg_stat_statements
```

The guide's aggressive autovacuum settings are for databases storing more than 500 GB, which a 75-minute run does not reach, so the defaults stay. Each run starts from a fresh database.

## Not applied, and why

- **Bulk trigger endpoints.** Producers in this workload send one document at a time, the way an upload-driven application does. Batching producers would be a change to the application, not a Hatchet setting.
- **Running the scheduler on its own engine instance.** That adds a machine. If calibration shows scheduler warnings on a single engine, it is revisited with the machine budget held equal across configurations.
- **Rate limits.** Experiment 1 has no downstream service to protect. Rate limits and rate-limit pause belong to experiment 2.

## Calibration

Run B alone at increasing `RATE` until Postgres can no longer keep up (sustained queue-wait growth, or database CPU pinned). Call the highest stable rate 100%, and record it with the calibrated settings above. All measured runs use that rate.

### Result (2026-10-23, Fly iad, v0.110.8)

**100% is 20 documents/s.** Every measured run uses `RATE=20`, `TENANT_MAX_RUNS=50`, `TRIGGER_SLOTS=20`, `BUFFER_SIZE=50`, `BUFFER_FLUSH=25ms`, `ENGINE_DB_CONNS=100`.

Each calibration run stepped the offered rate up in 5-minute steps. Raw data is in the run directories under `results/` (`calib-4`, `-5`, `-7`, `-9`).

| Offered (docs/s) | Completed: defaults, max_runs 50 | max_runs 200 | Tuned engine, max_runs 50 |
|---|---|---|---|
| 20 | 19.9 | 19.8 | 19.8 |
| 25 | 18.8 | – | 21.8 |
| 30 | 14.8 | 23.8 | 17.8 |
| 40 | 17 | 16.8 | – |
| 100 | 8 | – | – |

- **Postgres is the limit.** From 20 docs/s upward the Postgres machine runs at 185–195% CPU (both cores); the engine uses about one core and the workers and driver are nearly idle.
- **Past the limit, throughput falls.** The backlog is held as rows in `v1_concurrency_slot`, and rows read per second grow with it (about 50k at 20 docs/s, 400k–600k with a 50k backlog). The more is queued, the fewer documents complete.
- **`max_runs` 200 is not better.** It adds little throughput and makes small tenants 2–3x slower once the bulk tenant is backlogged (small-tenant p50 8–14 s against 3–4 s), so B keeps 50.
- **The engine buffers help past the limit** (about 15–20% more completed at 25 and 30 docs/s, smaller backlog), so B uses them. They do not move the knee.
