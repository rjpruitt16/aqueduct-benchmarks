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
- **C's small-tenant medians are fine but their tails are not.** Medians match B. The p99 is 5–10 times worse, because small tenants' documents were also refused while the law firm's queue was over budget, and the retries' backoff (up to tens of seconds) lands in the tail. Small tenants had 0–9 documents refused each (out of 1,680). Fair-share admission should not refuse a tenant sending 0.4 documents/s; this needs a look in Aquifer.
- **B varies a lot between runs.** B-2 finished everything and its law-firm latency was about half of B-1's. Two runs per configuration are not enough to rank B and C on latency.

## Fixed while running the pilot

These were harness bugs, not Hatchet or Aquifer behaviour. The runs from before the fixes were removed.

- The driver had one submission queue for all tenants, so when Aquifer refused submissions the small tenants' documents waited behind the law firm's inside the driver. Each tenant now has its own queue and submitters.
- Aquifer's config listed only Hatchet's API, so its completion webhooks to the driver fell under the 2 requests/s default for unknown hosts. The webhook backlog then tripped Aquifer's `webhook_backlog` admission rule. The driver now has its own upstream entry.

## Re-run of C with the fair-admission fix

The finding above was an Aquifer bug: past the backlog budget, Aquifer refused every request (`upstream_queue`), whatever queue it came from, so the law firm filling the budget also locked out small tenants. [aquifer#125](https://github.com/rjpruitt16/aquifer/pull/125) (`630501d`) keeps queues at or below their fair share entering past the budget, up to 1.25× it, and measures excess against the fair share so an oversized queue is refused every time before the budget fills. C-3 and C-4 ran 2026-10-25 with that Aquifer and otherwise the same settings. Raw data is in `results/pilot-{c3,c4}`.

| | C-1 | C-2 | C-3 (fix) | C-4 (fix) |
|---|---|---|---|---|
| Completed (of 96,000) | 94,492 | 95,713 | 95,818 | 86,335 |
| Refused by Aquifer, after 6 attempts | 330 | 287 | 182 | 254 |
| Refused, small tenants (each) | 0–3 | 1–9 | 0 | 0 |
| Refused, accounting | 9 | 8 | 0 | 0 |
| Unfinished after the 30 min drain | 1,178 | 0 | 0 | 9,411 |
| Law firm p50 / p99 in overload (min) | 32.9 / 52.1 | 31.8 / 49.4 | 20.4 / 37.9 | 42.1 / 60.0 |
| Small tenants p50 / p99 in overload (s) | 4.2 / 54.9 | 4.2 / 51.3 | 2.5 / 5.2 | 4.4 / 8.8 |
| Accounting p99 in overload (s) | 32.5 | 33.1 | 5.1 | 8.3 |

- **Only the law firm is refused now.** Small tenants and accounting had no refusals and no retries, and their overload p99 is back within B's range (5.9–10.2 s).
- **The spread between runs is still large.** C-4's law firm was much slower than C-3's and 9,411 of its documents were unfinished at the end, like B-1. The two pairs agree on fairness, not on throughput.
- Small tenants' recovery-phase p99 (9.5–12.7 s) is higher than B's (4.2–6.3 s), while the law firm's backlog drains.

The results copy for both runs failed on the one-line `DONE` file (written by hand afterwards; both drivers exited 0), which left both apps running for about 10 hours before they were destroyed. `run.sh` now reads `DONE` over ssh and stops a failed run's machines.

## Speed series: Aquifer paced below 100%

Eight more runs, 2026-10-25/26, same settings and Aquifer `630501d`: two more of B, and C with Aquifer forwarding at 90% (`AQUIFER_RPS=18`) and 80% (`AQUIFER_RPS=16`) of the calibrated rate. Raw data is in `results/speed-*`.

| | B-3 | B-4 | C90-1 | C90-2 | C90-3 | C80-1 | C80-2 | C80-3 |
|---|---|---|---|---|---|---|---|---|
| Completed (of 96,000) | 92,605 | 96,000 | 95,673 | 95,957 | 95,809 | 95,918 | 52,302 | 95,924 |
| Refused (by Aquifer in C; Hatchet `500` in B-3) | 396 | 0 | 327 | 43 | 191 | 82 | 136 | 76 |
| Unfinished at the end | 2,999 | 0 | 0 | 0 | 0 | 0 | 43,562 | 0 |
| Law firm p50 / p99 in overload (min) | 49.6 / 60.6 | 36.9 / 51.0 | 22.0 / 39.7 | 21.8 / 32.5 | 26.2 / 41.4 | 25.9 / 40.1 | 13.6 / 26.5 | 24.3 / 37.4 |
| Small tenants p50 / p99 in overload (s) | 6.7 / 78.2 | 4.6 / 8.7 | 2.4 / 4.0 | 1.4 / 2.6 | 3.2 / 6.7 | 1.7 / 3.0 | 2.3 / 5.7 | 1.7 / 16.6 |
| Accounting p99 in overload (s) | 232 | 8.5 | 4.1 | 2.5 | 6.7 | 2.9 | 5.4 | 16.9 |
| Postgres rows read/s, sustained (thousands) | 332 | 448 | 75 | 59 | 189 | 66 | 53 | 57 |
| Largest `v1_concurrency_slot` backlog (rows) | 51,977 | 44,776 | 7,939 | 89 | 11,809 | 117 | 35,685 | 426 |

- **Paced below 100%, Hatchet's own queue stays nearly empty.** In four of the six paced runs the `v1_concurrency_slot` backlog never passed 8,000 rows (89–426 in three of them), against 35,000–52,000 in every B run. Postgres reads 5–8 times fewer rows than in B (C90-3, 2–3 times).
- **That buys the small tenants' and accounting's latency back, and the law firm loses nothing.** Small-tenant p99 is 2.6–6.7 s in five of the six paced runs, against 5.9–78 s for B. The law firm's median is 22–26 min in the paced runs that finished, against 24–50 min for B: pacing below Postgres's limit did not cost it throughput.
- **Postgres CPU stayed at about 1.8–2.0 of its 2 cores, paced or not** (read from Fly's metrics during the runs up to C80-2; not saved with the results). The gain does not come from CPU headroom, as the open question below assumed. It comes from Hatchet's queue tables staying small, so each scheduling query reads less.
- **Two runs failed in Hatchet, one of each.** In B-3 Hatchet's API returned `500` for about 8 minutes (396 documents refused, 2,999 never started). In C80-2 Hatchet stopped completing jobs about 30 minutes in while Postgres sat idle, and 43,562 documents were unfinished at the end. The Fly logs were gone by the time the runs were looked at, so the cause is unknown; `run.sh` now saves them. C80-3 completed everything but had a 21 s gap in the small tenants' completions, which shows in its p99.
- Aquifer refused 0.05–0.35% of documents (the law firm's, after 6 attempts).

## Patch 2: Hatchet tells Aquifer how fast to send

Three runs of C, 2026-10-26, with the fork's pacing on (`hatchet-aquifer-api:v0.110.8-aqueduct-237971d`, `PACING=1`, target depth 1,000) and Aquifer allowed the full calibrated rate (`AQUIFER_RPS=20`). Hatchet counts its waiting concurrency slots once a second and advertises a rate that falls when the count is above 1,000 and rises when it is below 500. Raw data is in `results/pace-c-*`.

| | Pace-1 | Pace-2 | Pace-3 | C at 100%, no pacing (C2) | C80, best runs |
|---|---|---|---|---|---|
| Completed (of 96,000) | 95,884 | 95,735 | 95,913 | 95,713 | 95,918–95,924 |
| Refused by Aquifer | 116 | 265 | 87 | 287 | 76–82 |
| Law firm p50 / p99 in overload (min) | 24.5 / 36.4 | 25.2 / 38.2 | 23.8 / 35.3 | 31.8 / 49.4 | 24.3–25.9 / 37.4–40.1 |
| Small tenants p50 / p99 in overload (s) | 2.0 / 3.3 | 2.7 / 4.6 | 1.5 / 2.5 | 4.2 / 51.3 | 1.7 / 3.0–16.6 |
| Accounting p99 in overload (s) | 3.3 | 4.3 | 2.3 | 33.1 | 2.9–16.9 |
| Postgres rows read/s, sustained (thousands) | 51 | 52 | 53 | 232 | 57–66 |
| Largest `v1_concurrency_slot` backlog (rows) | 104 | 1,175 | 76 | 23,811 | 117–426 |

- **Hatchet's signal does the tuning by hand did.** Allowed 100% of the rate, Aquifer slowed itself to what Hatchet could take, and every run came close to the best hand-tuned 80% runs without the tuning: Hatchet's queue stayed at or near the target, Postgres read about a fifth of what it did without pacing, and small tenants' p99 stayed under 5 s. They didn't beat them everywhere: Pace-2's law-firm p99 (38.2 min against 37.4), accounting p99 (4.3 s against 2.9) and largest backlog (1,175 rows against 117–426) were worse.
- **All three runs agree**, unlike C80 and C90, where one run in three stalled or let the backlog grow. Three runs are not proof that pacing prevents stalls, but none happened.
- Nobody had to pick a number. The limit came from Hatchet's own queue, so it should follow Hatchet if its database grows; that is the resize experiment in [NEXT.md](NEXT.md).

## Open questions for the next runs

1. ~~Pace Aquifer below Postgres's limit (for example `AQUIFER_RPS=16`, 80%), so Hatchet's Postgres has headroom.~~ Done; see the speed series above.
2. ~~Why small tenants get `queue_fair_share` or `upstream_queue` 429s at all.~~ Fixed in aquifer#125; see the re-run above.
3. What stalls Hatchet in runs like C80-2 and B-3. The next runs keep Fly's logs.
4. The experiments in [NEXT.md](NEXT.md).
