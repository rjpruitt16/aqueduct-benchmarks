# Envoy and Aquifer together: gateway failures and durable admission

Status: designed and smoke-tested locally; **not run on Fly**. Paid runs need approval (see [PLAN.md](PLAN.md)).

## The question

Envoy and Aquifer were benchmarked separately ([envoy/RESULTS.md](../envoy/RESULTS.md), [hatchet/PILOT.md](../hatchet/PILOT.md)). This experiment asks whether they complement each other. Does Aquifer behind Envoy make end-to-end workflow reliability measurably better when the gateway or the backend fails, without adding latency or complexity that outweighs the gain? If Envoy alone does as well, that is the result.

## Premises

- **Aquifer manages retries; it isn't the source of truth for what happened.** Whether a document's work ran is recorded by Hatchet in Postgres, and by the driver's ledger, which hears every completion. Aquifer's job record says only what Aquifer accepted and last heard. Every duplicate count here comes from Hatchet's own runs per document, not from Aquifer.
- **Hatchet is already durable** once it accepts a run. Aquifer can only add value for work Hatchet hasn't accepted yet, or by changing how much retrying it takes to get there.
- **Hatchet can already reject duplicate triggers.** With workflow idempotency keyed by document, a second trigger for the same document is rejected instead of running twice. Every configuration has it on (`IDEMPOTENCY_TTL=3h` in the worker), so no configuration is exposed to duplicates Hatchet itself prevents. This was found in review; see [REVIEW.md](REVIEW.md).
- **Callers retry until their work is done**, as in the wild: exponential backoff with jitter, honoring `Retry-After`, with no attempt limit (`MAX_ATTEMPTS=1000000`). A caller stops retrying a trigger once its document has completed.
- **Aquifer runs at its default disk flush (100 ms).** Its `201` is sent before the job is on disk, and a crash can lose the jobs acknowledged since the last sync. That window is measured, not removed: the zero-flush setting is not used, because it is not how Aquifer is meant to run.

## Configurations

All three use the harness and workload of [hatchet/](../hatchet/): Hatchet v0.110.8 on Fly `iad`, Postgres on `performance-2x`, three workers, seven tenants (a law firm sending bulk, accounting, five small tenants), `RATE=20` documents/s (the calibrated 100%), the agreed 75-minute schedule (steady 50%, burst 300%, sustained 150%, recovery 50%) and a 60-minute drain.

| | Path | `run.sh` | Front door and API machines | Budget (performance-1x units) |
|---|---|---|---|---|
| **A** | Envoy → Hatchet | `D` | Envoy 1x; Hatchet API 2x (`API_VM=performance-2x`) | 3 |
| **B** | Envoy → Aquifer → Hatchet | `E` | Envoy 1x; Aquifer 1x; Hatchet API 1x | 3 |
| **C** | Aquifer → Hatchet | `C` | Aquifer 1x; Hatchet API 2x (`API_VM=performance-2x`) | 3 |

How each gateway is configured:

- **Envoy in A** is tuned the way the [Envoy benchmark](../envoy/README.md) tunes it, for Hatchet's capacity: per-tenant rate limits of 15 triggers/s under a total of 20/s, adaptive concurrency, admission control, circuit breakers and two retries of a 503.
- **Envoy in B** is the same configuration in front of Aquifer, with rate limits set only against abuse (100/s per tenant, 400/s in total). Aquifer paces Hatchet, so limiting to capacity twice would only add refusals.
- **Aquifer in B and C** is the one from the Hatchet pilot (`630501d`), paced by Hatchet's own queue depth (Patch 2, `PACING=1`). It retries each job until Hatchet takes it (`AQUIFER_MAX_RETRIES=-1`).

How the protocols differ:

- **A:** the caller's trigger goes through Envoy, and Hatchet's answer, with the run ID, comes back on the same request.
- **B and C:** the caller sends `POST /jobs` (through Envoy in B) and gets Aquifer's `201` with a job ID. Aquifer then triggers Hatchet and posts Hatchet's answer to the driver's webhook. **The `201` is an acknowledgment, not acceptance by Hatchet, and never a completion.** The driver records the two separately, and completion only from the workflow's last step.
- **In all three:** the driver's status polls go straight to Hatchet's API, so polling never uses up a gateway's limits or shows up in its counters.

**Redundancy.**

- **One Envoy and one Aquifer, no replicas.** Every configuration runs a single gateway of each kind, so a failure shows its full effect.
- **Production would run Envoy with at least two replicas.** Killing one would then affect only its own in-flight connections. These runs measure that per-replica effect; with replicas it would be smaller in A and B alike.
- **Aquifer has no equivalent.** It doesn't replicate jobs, so a second node would add capacity, not protection for jobs the first had acknowledged. Neither configuration is given replicas, and the results must not be read as what a redundant deployment would see.

## Scenarios

Every failure is injected by `run.sh` at the same second of the schedule in every configuration, and logged with its time in `events.txt`.

| | Failure | When | Injection | Applies to |
|---|---|---|---|---|
| **S0** | none (control) | – | – | A, B, C |
| **S1** | Envoy killed abruptly | 900 s, mid-burst | `SIGKILL` to Envoy's process; Fly's restart policy (`on-failure`) brings it back | A, B |
| **S2** | Envoy restarted gracefully | 900 s | `POST /drain_listeners?graceful` on Envoy's admin API, wait `--drain-time-s` (15 s), then `fly machine restart` | A, B |
| **S3** | Hatchet's API unavailable | 1500 s, sustained overload, for 5 minutes | `fly machine stop api`, then `start` | A, B, C |
| **S4** | Aquifer killed abruptly | 900 s | `SIGKILL` to Aquifer's process; it restarts on the same volume | B, C |

S4 was added in review: S1 and S2 never touch Aquifer, so they can't test what happens to work Aquifer has acknowledged. S0 measures what Aquifer costs when nothing fails.

## Hypotheses

| | Hypothesis | Null hypothesis |
|---|---|---|
| **S0** | B and C add a little latency to first acceptance (an extra hop and queue), and keep Hatchet's queue shorter. Completion latency is similar to A. | No difference from A in completion latency. |
| **S1** | In A, triggers in flight when Envoy dies are lost to the caller, who retries; Hatchet's idempotency stops duplicate runs, at the cost of 500s and more retries. In B, the jobs Aquifer already acknowledged are unaffected, because they're behind Envoy and continue while Envoy restarts. Only acknowledgments in flight retry. | Same work completed, same duplicates and the same retries in A and B. |
| **S2** | Graceful draining finishes most in-flight triggers, so A's and B's disruption is smaller than in S1 and similar to each other. Aquifer adds little here. | No difference from S1, or B differs from A. |
| **S3** | In A, callers are refused for 5 minutes and keep retrying (retry amplification); completions resume when the API returns. In B and C, callers are acknowledged throughout, Aquifer holds and retries the jobs, and callers retry far less. Completion times after the outage are similar, because Hatchet's capacity is the limit. | Retry amplification and completion times are the same in A and B. |
| **S4** | Jobs Aquifer acknowledged within its last unsynced flush window (up to about 100 ms, more under heavy writes) are lost. Jobs it had already handed to Hatchet are unaffected. Callers whose jobs were lost don't know it, because their acknowledgment said otherwise; the ledger shows them as acknowledged and never completed. A caller holding an open request retries; one that already got its `201` doesn't. | No acknowledged job is lost. |

## Measurements

From the driver's ledger, an independent record of every document from its first offer, plus Hatchet's own records and the gateways' counters, sampled every 2 s by the driver:

- **Volume:** documents offered; workflows completed, which means Hatchet ran the last step, not that any gateway answered.
- **End-to-end latency:** p50, p95 and p99 from first offer to completion, by tenant and phase (`p50_ms_by_phase`, `p95_ms_by_phase`, `p99_ms_by_phase`).
- **Time to first acceptance, separately:** Hatchet's answer in A, Aquifer's `201` in B and C (`accept_*_ms_by_phase`). Also the time until Hatchet's run ID was known (`to_run_id_*_ms`).
- **The caller's retrying:** retries and total backoff time, by tenant (`retries`, `retried_documents`, `backoff_seconds`).
- **Retries inside the path:**
  - Envoy's own retries and upstream resets (`cluster.backend.upstream_rq_retry`, `upstream_rq_rx_reset`, `upstream_rq_tx_reset`), summed across Envoy restarts;
  - Aquifer's requests to Hatchet, by status (`aquifer_dispatch_attempts_total`), summed across Aquifer restarts.
- **Where unfinished work stopped** (`acceptance`, kept separate):
  - acknowledged by Aquifer but never reached Hatchet;
  - accepted by Hatchet but never completed;
  - never accepted.
- **Acknowledged jobs lost:** acknowledged by Aquifer and never completed by the end of the drain.
- **Duplicate executions:** Hatchet's own runs per document (`duplicates.json`, from `v1_dags_olap`), plus completion callbacks seen twice (`duplicate_completions`). Also documents that completed while their trigger was still being retried (`settled_while_retrying`).
- **Recovery times, measured from the injection:** when the gateway first fails its readiness check and is next ready (`envoy-ready`, `aquifer-ready` in `scrape.jsonl`), when completions resume, and when the backlog is back to its pre-failure level.
- **Per tenant:** throughput, and Jain's index of fairness over the overload.

The [results template](RESULTS-TEMPLATE.md) lists every one of these for every run. [`ana/gateway.py`](ana/gateway.py) prints them from a run's directory.

## What this can't show

- **Losing a disk or a host.** Every Aquifer restart here keeps its volume, so nothing here supports a claim about either. Aquifer keeps one copy of its jobs on one disk; losing it loses them. That needs its own experiment ([research note](../hatchet/research/gateway-failures.md#recommended-experiments), experiment 1).
- **Redundant deployments** (see above).
- **Generality.** One backend (Hatchet, itself durable), one region, one workload, and two rounds per cell.
