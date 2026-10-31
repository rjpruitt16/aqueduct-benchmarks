# Review of the design, before any paid run

A skeptical pass over [DESIGN.md](DESIGN.md), looking for anything that would unfairly favor one side, conflate measures, or let a result be misread. Each finding says what was changed. Findings confirmed by running something say how.

## Does the design disadvantage Envoy?

1. **Hatchet can already reject duplicate triggers, and the first draft didn't use it.** *Confirmed by reading the code.* Hatchet v0.110.8 supports workflow idempotency: a key expression over the input, with a TTL. Its Go SDK exposes it as `WithWorkflowIdempotency`, and the schema has `idempotency_key` on runs, tasks and DAGs. Without it, A would show duplicate runs after an Envoy crash that Hatchet itself would have prevented, an Aquifer "win" that doesn't exist. **Fixed:** the worker takes `IDEMPOTENCY_TTL`, keyed by `input.doc_id`, and every configuration runs with it on.
2. **Hatchet's REST API answers an idempotency collision with HTTP 500.** *Confirmed on local Hatchet:* triggering the same document twice gave `200`, then `500 {"message":"Internal Server Error"}`. The REST handler maps only the gRPC codes `InvalidArgument` and `ResourceExhausted`, and `AlreadyExists` falls through. A REST caller therefore can't tell "already submitted" from a server fault, and will keep retrying. That cost is real for anyone calling Hatchet's REST API through Envoy, so it stays in A. But it must be reported as Hatchet's behavior, not Envoy's. **Fixed:** the driver stops retrying a trigger once the document's completion callback has arrived (`settled_while_retrying`), as a careful client watching its results would. Without that, A's callers would retry collisions until the run ended.
3. **Envoy limiting to capacity in B would double the refusals.** **Fixed:** in B, Envoy's limits are set against abuse only (100/s per tenant, 400/s in total); Aquifer paces Hatchet.
4. **A single Envoy is not how Envoy is run.** With replicas, a SIGKILL takes out only one replica's in-flight connections. **Kept, and stated:** both A and B run one Envoy, so the per-replica effect is measured equally. DESIGN.md says the results are not those of a redundant deployment.
5. **Graceful restart needs the right procedure.** Envoy doesn't drain on `SIGTERM`; it needs `POST /drain_listeners?graceful` and a drain period first. **Fixed:** S2 drains through the admin API, waits `--drain-time-s` (15 s), then restarts.

## Does Hatchet already solve the tested problem?

6. **For work Hatchet has accepted, yes.** An Envoy crash in A can only affect triggers in flight on the dying connection. Hatchet has either committed a run or not; the caller doesn't know which and retries; idempotency stops a second run. So the expected difference in S1 is retries and 500s, not lost work. **Kept, with that expectation stated** in DESIGN.md. A null result there is a likely and legitimate outcome.
7. **S1 and S2 never touch Aquifer.** In B, Envoy sits in front of Aquifer. Killing Envoy leaves Aquifer, and every job it has already acknowledged, untouched. Those scenarios can show that work behind the edge keeps moving, but not whether Aquifer keeps work through its own failure. **Fixed:** S4 (SIGKILL Aquifer) was added for B and C.
8. **Aquifer's acknowledgment is not durable at the default flush.** Its `201` precedes the disk write by up to about 100 ms, more under heavy writes. **Kept at the default**, per the experiment's owner: the zero-flush setting isn't how Aquifer is meant to run. S4 measures the loss instead. No claim about disk loss or host loss is made, because no scenario tests either.

## Are acceptance and completion kept apart?

9. **Three different events, three fields.** In B and C:
   - Aquifer's `201` is `accepted`;
   - Hatchet's acceptance arrives later by webhook as `triggered`;
   - completion is the workflow's last step calling the driver back.

   In A, `accepted` and `triggered` are the same moment. **Fixed:** the summary splits unfinished work into "acknowledged by Aquifer, never reached Hatchet", "accepted by Hatchet, never completed" and "never accepted". First-acceptance latency is reported separately from completion latency, and the analysis labels every latency by its endpoints.
10. **Status polls must not pass through a gateway.** They'd consume Envoy's limits and inflate its counters in A. **Already so:** polls go straight to Hatchet's API in every configuration.

## Are the same jobs counted?

11. **Same workload, same counter.** Every configuration offers the same schedule and the same seven tenants. The workload is deterministic, but document IDs are generated per run, so runs are compared by counts and distributions, not document by document. Duplicates are counted from Hatchet's own records (one `v1_dags_olap` row per workflow run, grouped by `doc_id`), so they don't depend on which gateway saw what.

## Does the failure timing favor one architecture?

12. **Same second in every configuration.** Every failure is injected at the same second of the schedule: 900 s (mid-burst) for kills and restarts, and 1500–1800 s for the API outage.
13. **One injection time is still a sample, not a sweep.** A mid-burst kill catches A with many triggers in flight and B with a large Aquifer backlog. A kill in a quiet phase might show nothing in either. **Kept:** two rounds at the same time, stated as a limit, plus the S0 control.

## Do budgets distort the comparison?

14. **B has a third machine in the path.** **Fixed:** each configuration gets three performance-1x units for its front door and Hatchet's API. A and C put the spare unit into Hatchet's API (performance-2x), and B spends it on Aquifer. Postgres, the engine, workers and driver are identical.

## Found by the local smoke test

15. **Scrape entries were split on `|`**, which Envoy's stats filter (a regular expression) also contains, so the Envoy counters weren't sampled. **Fixed:** entries are now "name url pattern", separated by spaces.
16. **Hatchet accepted a trigger for a workflow name nobody had registered** (HTTP 200 with a run ID). Not relevant to this experiment beyond noting that a `200` from Hatchet's REST trigger doesn't prove the workflow exists.

## Still open

- **The Fly injections are checked only by the paid smoke part** (`series.sh smoke`, 3 short runs). Locally, Envoy is killed with `kill -9` on the container's process and restarted by Docker. On Fly, `run.sh` kills it over `fly ssh` and Fly restarts it.
- **OLAP tables are written asynchronously.** `duplicates.json` is read when the driver finishes, after the drain. A run cut off mid-drain could undercount.
