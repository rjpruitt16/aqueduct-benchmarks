# aqueduct-benchmarks

Reproducible benchmarks of [Aquifer](https://github.com/rjpruitt16/aquifer) in front of popular open-source software under congestion: bursts, overloaded databases, slow downstream services, and capacity that takes time to arrive.

Each benchmark asks whether Aquifer's admission control, pacing and per-tenant fairness measurably help a real system, compared with that system's own controls properly tuned. The goal is to find where Aquifer helps and where it doesn't, not to show that the target is broken.

## Targets

| Target | Status |
|---|---|
| [Hatchet](hatchet/) | In progress: harness being built |

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
