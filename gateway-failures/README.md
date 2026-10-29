# gateway-failures: Envoy and Aquifer together

Does putting Aquifer behind Envoy make Hatchet workflows more reliable when a gateway or the backend fails? Or does Envoy alone do as well, and Aquifer only add a hop?

Three configurations on the same budget, in front of the same Hatchet:

- **A:** Envoy → Hatchet
- **B:** Envoy → Aquifer → Hatchet
- **C:** Aquifer → Hatchet

Four failures, plus a control: S1 an Envoy SIGKILL, S2 a graceful Envoy restart, S3 Hatchet's API down for 5 minutes, and S4 an Aquifer SIGKILL.

**Status:** designed, reviewed, smoke-tested locally. **No paid run yet.**

| Document | What it holds |
|---|---|
| [DESIGN.md](DESIGN.md) | The question, premises, configurations, scenarios, hypotheses and every measurement |
| [REVIEW.md](REVIEW.md) | The skeptical review and what it changed, including that Hatchet already prevents duplicate runs when its idempotency is on, and that its REST API reports a duplicate as HTTP 500 |
| [PLAN.md](PLAN.md) | The execution plan, a $45 budget and the cleanup guarantees |
| [RESULTS-TEMPLATE.md](RESULTS-TEMPLATE.md) | Every metric, for every configuration and scenario |

Harness:

- [`local/smoke.sh`](local/smoke.sh): one configuration and one failure on a single machine with Docker.
- [`deploy/series.sh`](deploy/series.sh): the Fly series, with [`hatchet/deploy/fly/run.sh`](../hatchet/deploy/fly/run.sh) configurations `D`, `E` and `C`.
- [`ana/gateway.py`](ana/gateway.py): reports a run in the template's terms.

The driver, worker, sampler and Fly scripts are shared with [hatchet/](../hatchet/).

The premise everything rests on: **Aquifer manages retries; it isn't the source of truth for whether an action happened.** Hatchet's records and the driver's ledger decide what ran. Every service still keeps its own database and stays idempotent on the key it is handed.

## Local smoke results

Checks of the plumbing at 4 documents/s on one machine, not results. See [PLAN.md](PLAN.md) for the pass criteria.

_(filled in below as each check runs)_
