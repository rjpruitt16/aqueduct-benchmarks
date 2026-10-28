# aqueduct-benchmarks

Reproducible benchmarks of [Aquifer](https://github.com/rjpruitt16/aquifer) in front of popular open-source software under congestion: bursts, overloaded databases, slow downstream services, and capacity that takes time to arrive.

Each benchmark asks whether Aquifer's admission control, pacing and per-tenant fairness measurably help a real system, compared with that system's own controls properly tuned. The goal is to find where Aquifer helps and where it doesn't, not to show that the target is broken.

## Targets

| Target | Status |
|---|---|
| [Envoy](envoy/) | Done: five load regimes on Fly, from steady load to retries stacked across a chain of services and capacity that arrives late. [Results](envoy/RESULTS.md) |
| [Hatchet](hatchet/) | Harness and Fly run script built. Two reported Hatchet issues confirmed on v0.110.8 ([reproductions](hatchet/repro/)). No measured runs published yet |

## What the Envoy series found

Envoy and Aquifer do different jobs. Envoy routes and refuses: when the backend is full, it turns work away and the caller has to retry. Aquifer holds the work, recognizes a resend by its idempotency key, gives every tenant an equal turn, and paces the backend at the rate the backend advertises. Most of what the series shows comes from that difference, and it suggests running them together, with Envoy at the edge and Aquifer behind it. That combination has not been measured yet.

- **When retries stack up across services, the work isn't repeated.** With four services each retrying 3 times, the backend behind Envoy spent 55–58% of its executions redoing work it had already done. Behind Aquifer, about 0.1%. The same backend completed about 2.2 times as many requests.
- **When callers retry until their work is done, they need one request per job.** Envoy needed 3.5 requests per job and gave more capacity to the tenants that retried hardest. Aquifer needed 1.0, and same-size tenants got the same share whatever their retry style or region.
- **When capacity arrives late, no work is lost.** Aquifer ran 100% of the jobs at every delay. Envoy, with limits sized for the capacity after scaling, ran 95%, 88% and 67% as capacity arrived 3, 5 and 10 minutes into the overload.
- **Callers should come back when Aquifer says, not on a backoff timer.** A caller using jittered exponential backoff slept through 4–5% of results that were ready in time. Callers that held the stream, or polled when Aquifer told them to, missed none.

Where it doesn't help, or the comparison is limited:

- **A team can build most of this itself.** Envoy plus idempotency keys in Redis, a fair queue and a dispatcher (about 500 lines) matched Aquifer under stacked retries. What Aquifer saves is building and owning that.
- **It needs a stable key and the backend's headers.** Without either, it lost most of its advantage.
- **Large tenants wait longer.** Equal turns mean a tenant sending half the traffic can be held to about a sixth of the capacity while six tenants compete.
- **For callers that give up after a fixed time, it's a tie at best.** With capacity 10 minutes away, Envoy answered more of the callers that gave themselves only 5 minutes. Aquifer ran all the work, for callers that come back.
- **Effort wasn't even, and the backend is synthetic.** The last round reran Aquifer with a fix and three client variants, but not Envoy. The backend has a fixed service time and no database.

Raw data, the harness and every caveat are in [envoy/](envoy/). The design documents explain each regime before its results.

## How every benchmark works

1. **Confirm the reported issues still happen** on a pinned version of the target before testing anything against them.
2. **Run the target stock, then tuned** with its own controls, following a written tuning protocol published with the results.
3. **Run the tuned target with Aquifer**, on the same total machine budget (Aquifer's machine counts).
4. **Count every job from when it is first offered**, including time spent waiting in Aquifer, jobs turned away, and jobs that time out before they ever start. A job only counts as accepted once it is durably stored.
5. **Repeat each comparison** and report the spread across runs, not just the best one.
6. **Publish wins and losses together,** with exact versions, configuration, resources and raw results.

Known bugs in a target are measured as part of every run, but a change in how often they occur is never claimed as an Aquifer fix.

## Running

Each target folder has its own instructions. Measured runs use [Fly.io](https://fly.io), and every machine is created for a run and destroyed afterward. Small-scale local runs use Docker Compose.

## License

Apache 2.0. Targets keep their own licenses; this repository contains only harness code, configuration and results.
