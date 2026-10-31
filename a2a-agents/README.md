# A2A agents across shared resources

Do agents that share a model, a database and a sandbox finish their work
sooner, and with less waste, when Aquifer holds the work than when each agent
retries on its own or when a queue and dispatcher sit in front?

A coordinator hands each issue to three agents over A2A (research, coding,
verify). Each issue needs model calls, database calls and sandboxed test runs,
and a wrong first fix sends it back for another round. The same issues run in
three configurations:

| | How agents reach a resource |
|---|---|
| **A** | directly, retrying with exponential backoff and jitter, honoring Retry-After |
| **B** | through a Redis queue and a dispatcher that holds each resource at its capacity |
| **C** | through Aquifer's `/proxy`, one upstream per resource with the same limits as B |

[DESIGN.md](DESIGN.md) has the workflow, scenarios, measurements and
acceptance criteria. [AUDIT.md](AUDIT.md) covers Aquifer's A2A adapter.
[PLAN.md](PLAN.md) has the status, the inventory and the pilot's budget.

## Local smoke test

`local/smoke.sh <A|B|C>` runs one configuration on one machine with Docker
and Go, using the stand-in model. Capacity is tight on purpose: 2 model
sequences, 2 sandbox slots, a database pool of 4. Eight workflows start at
once (one per issue; three need a second round), from a big tenant (6) and
two small ones (1 each). It checks the plumbing and the accounting, not
performance: one run per configuration, on a Fly Sprite with a slow disk.

Results, 2026-10-29:

| | Completed | p50 | p99 | Resource requests per workflow | Sandbox executions |
|---|---|---|---|---|---|
| A | 8/8 | 28 s | 46 s | 12.6 | 11 |
| B | 8/8 | 37 s | 48 s | 6.5 | 11 |
| C, first run | 8/8 | 134 s | 155 s | 8.3 | 18 |
| C, direct attempt timeout 60 s | 8/8 | 56 s | 76 s | 7.5 | 11 |

- **Same work everywhere.** Every configuration finished all eight issues with
  byte-identical final fixes, and the sandbox ran the 11 executions the work
  needs, with no repeats in A, B and the C rerun (acceptance criterion 3).
- **A spends requests on refusals.** Most of A's extra requests are sandbox
  calls refused with 429 and retried (60 sandbox requests for 11 executions).
- **C's first run repeated work.** Aquifer's `/proxy` gives a direct attempt 3 s
  by default. A test run takes 5–20 s, so Aquifer cut runs off, the sandbox
  killed them, and Aquifer ran them again: 7 extra executions. The smoke test
  now sets `AQUIFER_PROXY_DIRECT_ATTEMPT_TIMEOUT_SECONDS=60`. Queued dispatches
  also have a fixed 30 s client timeout in Aquifer, with no setting, which a
  test run under contention can exceed.
- **C's remaining time is Retry-After.** At startup the Sprite's disk lagged
  1–3 s on WAL syncs, and Aquifer refused 8 jobs. Its Retry-After doubles with
  each consecutive refusal (5, 10, 20, 40, 60 s), so a stall of about 3 s told
  callers, small tenants included, to wait 40–60 s. That's 230 s of the run's
  client waiting. Draft issue: [aquifer-retry-after-issue.md](aquifer-retry-after-issue.md).

These numbers say the harness works and point to two Aquifer behaviors to
watch. They don't compare the configurations: a pilot on real hardware does.
