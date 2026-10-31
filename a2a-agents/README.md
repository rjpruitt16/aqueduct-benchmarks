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

## GPU pilot, 2026-10-31

Qwen2.5-7B-Instruct on vLLM v0.31.0, one A40 on RunPod (about 3 h 20 min,
$0.59/h); the agents, Redis, Aquifer, mock CI and the sandbox on one Fly
performance-4x in ord. Three scenarios, each run twice per configuration: 8
minutes of load, then up to 4 minutes for workflows to finish. Calibration
measured 2.13 workflows/s and a first-round fix rate of 100%, so no issue
needed a second round. The model was at most 38% busy during calibration, so
the capacity is extrapolated. Full tables: [results/gpu2/REPORT.md](results/gpu2/REPORT.md);
the CPU rehearsal with the stand-in model: [results/reh2/REPORT.md](results/reh2/REPORT.md).

Means of the two runs:

| | A | B | C |
|---|---|---|---|
| normal: p50 / p99 | 53 / 104 s | 53 / 104 s | 53 / 104 s |
| burst: p50 / p99 | 163 / 328 s | 121 / 310 s | 122 / 310 s |
| burst: small tenants p99 | 245 s | 148 s | 149 s |
| burst: requests per workflow | 17.4 | 10.0 | 10.0 |
| burst: CI requests refused | 5,318 | 394 | 84 |
| flood: small tenants p99 | 308 s | 104 s | 106 s |
| flood: big tenant p50 | 212 s | 300 s | 302 s |
| flood: failed / unfinished of 1,530 | 525 / 0 | 0 / 566 | 0 / 577 |

- **Normal load: no difference.** All three configurations finished every
  workflow with the same latency and the same requests.
- **Bursts: B and C beat A, and tie with each other.** A's retries cost it 7
  extra requests per workflow and about 13,900 s of backoff per run. C's one
  measurable edge over B is downstream: Aquifer paced to mock CI's advertised
  limits, which refused it about 5× less often than it refused B's dispatcher.
- **Flood: B and C protect the small tenants; the big tenant pays.** A's
  failures are HTTP 524 from RunPod's HTTP proxy, which cuts a request off
  after about 100 s: A sent everything at once, about 300 requests waited in
  vLLM's queue, and `internal/resource/client.go` retries only 429 and 503. A
  direct connection to vLLM would not fail this way, so A's flood numbers
  partly measure the proxy. B's and C's unfinished workflows were all the big
  tenant's, still queued when the 4 minutes ran out. Completion counts are not
  comparable across configurations, since A dropped work early.

B is a careful queue and dispatcher built for this benchmark, and it sees all
of the traffic. Against it, Aquifer matched latency and requests without code
of its own, and drew fewer downstream refusals. Whether that holds when each
team runs its own queue is
[issue #13](https://github.com/rjpruitt16/aqueduct-benchmarks/issues/13).

Setup lessons: vLLM's `-cu129` image can't start (its torchcodec needs
`libnvrtc.so.13`; the plain v0.31.0 tag is CUDA 13 and works); RunPod's
`allowedCudaVersions` filter matches exact versions and REST v1 accepts none
past 13.0, so any filter excluded the CUDA 13.2 hosts; the run was driven by
a small Fly machine (`deploy/ctl.sh`) that deleted the pod and the app when
the series ended.
