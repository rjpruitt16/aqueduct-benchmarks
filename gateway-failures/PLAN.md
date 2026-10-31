# Execution plan and budget

Nothing on Fly runs until this plan is approved. Steps 1 and 2 cost nothing.

## 1. Local smoke tests (free)

On a machine with Docker and Go (the Sprite), against local Hatchet (`hatchet/deploy/local/setup.sh`). Each configuration runs with each failure that applies to it, at a low rate (4 documents/s) on a 140-second schedule:

```sh
gateway-failures/local/smoke.sh A envoy-kill    # and envoy-drain, api-stop
gateway-failures/local/smoke.sh B envoy-kill    # and envoy-drain, api-stop, aquifer-kill
gateway-failures/local/smoke.sh C aquifer-kill  # and api-stop
python3 -I gateway-failures/ana/gateway.py .smoke/*/out
```

**Pass criteria:**

- **Accounting:** every configuration completes its documents, and every document is accounted for in the ledger (completed, or placed in one of the acceptance buckets).
- **Instrumentation:** the gateways' readiness and counters are scraped, and `duplicates.json` is written.
- **Injection:** each failure appears in `events.txt` at the right time.

The [README](README.md) records what these runs show; they are checks, not results.

## 2. Review (done)

[REVIEW.md](REVIEW.md): findings and the fixes made before any paid run.

## 3. Fly smoke (about $2, needs approval)

`series.sh smoke` runs three short runs (a tenth of the schedule, 15-minute drain), and checks the Fly injection commands:

- B with an Envoy SIGKILL (`run.sh` kills Envoy over `fly ssh`; Fly restarts it);
- A with Hatchet's API stopped;
- C with an Aquifer SIGKILL.

Each run is about 25 minutes on about 10 machines.

## 4. The series (about $32, needs approval)

`series.sh round 1`, then `series.sh round 2`. Each round is 12 runs:

| Scenario | A | B | C |
|---|---|---|---|
| S0 control | ✓ | ✓ | ✓ |
| S1 Envoy SIGKILL | ✓ | ✓ | – |
| S2 Envoy graceful restart | ✓ | ✓ | – |
| S3 Hatchet API stopped 5 min | ✓ | ✓ | ✓ |
| S4 Aquifer SIGKILL | – | ✓ | ✓ |

**Machines per run, all dedicated CPU:**

- Postgres, Hatchet's engine and the driver: performance-2x each.
- Three workers: performance-1x each.
- The front door and Hatchet's API: 3 performance-1x units.

At Fly's list prices that's about $0.55 an hour. A run takes up to about 2.4 hours: setup, the 75-minute schedule and up to 60 minutes of drain. So about $1.30 a run at most, and 24 runs come to about $32.

## Budget and bounds

- **Maximum budget: $45**, covering the Fly smoke ($2), both rounds ($32), and up to about $11 for reruns of runs lost to Fly API timeouts or a Sprite restart. If the estimate would pass $45, the series stops and asks.
- **Bounded execution:**
  - `series.sh` runs at most `PARALLEL=4` at once;
  - it starts at most `MAX_RUNS=14` runs per invocation;
  - a run with results already in place is skipped.
- **Cleanup guarantees** (from [hatchet/deploy/fly/reap.sh](../hatchet/deploy/fly/reap.sh)), applied every 4–5 minutes by a watcher that runs from outside the Sprite:
  - an app whose orchestration died is copied if it finished, and destroyed;
  - an app whose setup never finished is destroyed after 10 minutes;
  - no app lives past 4 hours, whatever its state.

  The watcher also restarts Docker on the Sprite and checks that the orchestrators are alive. The Sprite has restarted on its own twice: that loses runs, not money.
- **Wall time:** about 7 hours per round at 4 runs in parallel; both rounds in about 14 hours.

## After the runs

`ana/gateway.py` on every run. Results go into [RESULTS-TEMPLATE.md](RESULTS-TEMPLATE.md)'s tables, in a new `RESULTS.md`, including negative and null results. Raw data is committed in `hatchet/results/gw-*`.
