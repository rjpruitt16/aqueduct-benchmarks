# Pilot: B (tuned Hatchet) against C (Hatchet behind Aquifer)

Two runs of each configuration, 2026-10-24, Fly `iad`, Hatchet v0.110.8, Aquifer `da18e04`. Every run uses the calibrated settings in [TUNING.md](TUNING.md): `RATE=20` (100% = 20 documents/s), the agreed 75-minute schedule (steady 50%, burst 300%, sustained 150%, recovery 50%), and a 30-minute drain. In C, Aquifer forwards to Hatchet at 20 requests/s with 32 in flight. Raw data is in `results/pilot-{b1,b2,c1,c2}`.

B-1 and C-1 overlapped in time with B-2 and C-2 on separate Fly apps (separate dedicated-CPU machines).

## Results

"Overload" is the burst and sustained phases together. Small tenants are `small-1` to `small-5`; their p99 is the worst of the five.

| | B-1 | B-2 | C-1 | C-2 |
|---|---|---|---|---|
| Completed (of 96,000) | 92,454 | 96,000 | 94,492 | 95,713 |
| Refused by Aquifer, after 6 attempts | – | – | 330 | 287 |
| Unfinished after the 30 min drain | 3,546 | 0 | 1,178 | 0 |
| Law firm p50 / p99 in overload (min) | 41.7 / 54.6 | 23.7 / 31.5 | 32.9 / 52.0 | 31.7 / 49.4 |
| Small tenants p50 / p99 in overload (s) | 5.1 / 10.2 | 2.9 / 5.9 | 4.0 / 54.8 | 4.1 / 51.2 |
| Accounting p99 in overload (s) | 9.6 | 5.5 | 32.5 | 33.0 |
| Postgres rows read/s, burst / sustained (thousands) | 326 / 463 | 512 / 593 | 165 / 292 | 157 / 309 |
| Largest `v1_concurrency_slot` backlog (rows) | 46,672 | 34,987 | 25,425 | 23,811 |
| Postgres CPU in overload (of 2 cores) | 1.95 | 1.86 | 1.91 | 1.90 |

No run had duplicate completions or `never_started` documents.

## What it shows

- **Aquifer halves Postgres's read load, but not its CPU.** With Aquifer in front, Hatchet's queue table holds about 40% fewer rows and Postgres reads about half as many rows per second. Postgres stays CPU-bound anyway, because Aquifer forwards at the calibrated 100%, which is exactly what Postgres can sustain. Completions and the law firm's latency are within the spread of the two B runs.
- **C refuses about 0.3% of documents.** Aquifer's admission control (`upstream_queue` and `queue_fair_share`) answers 429 while its queue for Hatchet is past its backlog budget. The driver gives up after 6 attempts, like any bounded producer. B accepts everything and falls behind instead (B-1 still had 3,546 documents unfinished at the end).
- **C's small-tenant medians are fine but their tails are not.** Medians match B. The p99 is 5–10 times worse, because small tenants' documents were also refused while the law firm's queue was over budget, and the retries' backoff (up to tens of seconds) lands in the tail. Small tenants had 0–9 documents refused each (out of 336). Fair-share admission should not refuse a tenant sending 0.4 documents/s; this needs a look in Aquifer.
- **B varies a lot between runs.** B-2 finished everything and its law-firm latency was about half of B-1's. Two runs per configuration are not enough to rank B and C on latency.

## Fixed while running the pilot

These were harness bugs, not Hatchet or Aquifer behaviour. Data from before the fixes is kept in `results/*-aborted*` and is not used above.

- The driver had one submission queue for all tenants, so when Aquifer refused submissions the small tenants' documents waited behind the law firm's inside the driver. Each tenant now has its own queue and submitters.
- Aquifer's config listed only Hatchet's API, so its completion webhooks to the driver fell under the 2 requests/s default for unknown hosts. The webhook backlog then tripped Aquifer's `webhook_backlog` admission rule. The driver now has its own upstream entry.

## Open questions for the next runs

1. Pace Aquifer below Postgres's limit (for example `AQUIFER_RPS=16`, 80%), so Hatchet's Postgres has headroom. This is the configuration Aquifer is meant for, and it costs Hatchet's throughput a little to buy the latency back.
2. Why small tenants get `queue_fair_share` or `upstream_queue` 429s at all (an Aquifer question).
3. More runs per configuration, given B's spread.
