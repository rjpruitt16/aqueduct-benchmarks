# Results template

Every number comes from `python3 -I gateway-failures/ana/gateway.py hatchet/results/gw-*`. Each cell is the range over two rounds. A cell that doesn't apply is "–". A cell that wasn't measured is "not measured", never left blank.

"Completed" always means Hatchet ran the workflow's last step. An acknowledgment from Aquifer or Envoy is never counted as completion.

## Per scenario (repeat for S0 to S4)

Scenario: ______  Injected at ______ s (from `events.txt`)

| Metric | Source | A: Envoy → Hatchet | B: Envoy → Aquifer → Hatchet | C: Aquifer → Hatchet |
|---|---|---|---|---|
| Documents offered | ledger | | | |
| Workflows completed (share of offered) | ledger, completion callback | | | |
| End-to-end p50 / p95 / p99, first offer to completion, all tenants, overload | `p*_ms_by_phase.overload` | | | |
| First acceptance p50 / p99 (Hatchet's answer in A, Aquifer's 201 in B and C) | `accept_p*_ms_by_phase` | | | |
| Time until Hatchet's run ID was known, p50 / p99 | `to_run_id_p*_ms` | | | |
| Caller retries (total, documents retried) | `retries`, `retried_documents` | | | |
| Caller backoff time (total; p99 per document) | `backoff_seconds`, `backoff_p99_ms_by_phase` | | | |
| Envoy's own retries; upstream resets; connections destroyed with requests active | `scrape.jsonl` (envoy-stats, summed across restarts) | | | – |
| Aquifer's requests to Hatchet, by status; per document | `scrape.jsonl` (aquifer-metrics) | – | | |
| Acknowledged by Aquifer, never reached Hatchet | `acceptance.acked_not_triggered` | – | | |
| Accepted by Hatchet, never completed | `acceptance.triggered_not_completed` | | | |
| Never accepted | `acceptance.never_accepted` | | | |
| **Acknowledged jobs lost** (acknowledged by any front door, never completed after the drain) | the two rows above | | | |
| Duplicate executions: documents Hatchet ran more than once; extra runs | `duplicates.json` | | | |
| Duplicate completion callbacks | `duplicate_completions` | | | |
| Documents completed while their trigger was still being retried | `settled_while_retrying` | | | |
| Gateway not ready → ready again (seconds after the injection) | `scrape.jsonl` (envoy-ready, aquifer-ready) | | | |
| Completions resumed (seconds after the last event) | `intervals.jsonl` | | | |
| Backlog back to its pre-failure level (minutes after the last event) | `intervals.jsonl` | | | |
| Postgres CPU; documents per Postgres CPU-second | `cpu.json` | | | |
| Fairness (Jain's index, overload) | `jain_overload` | | | |

## Per tenant (for each scenario)

| Tenant | A: completed; p50 / p99 | B: completed; p50 / p99 | C: completed; p50 / p99 | A / B / C: retries; backoff (s) |
|---|---|---|---|---|
| law-firm | | | | |
| accounting | | | | |
| small-1 … small-5 (range) | | | | |

## Verdicts to fill in, in this wording

- **S0:** Aquifer behind Envoy costs ___ at the median and ___ at p99 compared with Envoy alone.
- **S1:** after an Envoy crash, acknowledged work lost: A ___, B ___. Duplicate runs: A ___, B ___. Caller retries: A ___, B ___. Whether B kept work A lost: ___.
- **S2:** graceful restart compared with S1: ___. Whether Aquifer added anything beyond Envoy's draining: ___.
- **S3:** retry amplification during a 5-minute Hatchet outage: A ___ requests per document, B ___. Time to clear the backlog: A ___, B ___.
- **S4:** jobs Aquifer acknowledged and lost when killed: B ___, C ___. Duplicate runs from Aquifer re-sending work Hatchet had already taken: ___.
- **Overall:** whether Envoy + Aquifer gave a measurable reliability advantage, a cost, or no difference: ___.
