# Event ingestion: design for review

Status: proposal, nothing built. Needs sign-off before code or spend.

## The question

Hatchet keeps its queue in Postgres, and its scheduler reads those tables on every pass. If a deeper backlog makes each pass more expensive, then a Hatchet that accepts everything spends more of its CPU per event exactly when it is behind, and gets slower the further behind it falls. The two ways to keep that backlog out of Hatchet are circuit breaking (refuse, so the sender holds the backlog and redelivers) and durable queuing (accept into Aquifer's disk, forward at the rate Hatchet advertises).

The document runs don't settle this. Their backlog peaked around 50,000 `v1_concurrency_slot` rows, each document was four 50 ms steps, and Postgres did about the same work per CPU-second with or without Aquifer: 8.6–11.6 documents per Postgres CPU-second for B, 8.7–11.2 for C. That is evidence *against* the theory at that depth and task length. Event ingestion is a different shape: one short task per event, far more events per second, and a backlog that can reach millions during a sale. Scheduling overhead is a much larger share of the work there, so if a backlog cost exists, this is where it would show.

## Step 0: does backlog depth cost anything? (local, no spend)

Run this before building the rest. If it comes out flat, the theory is wrong, we write that down and stop.

1. Start Hatchet (`deploy/local`) with workers stopped.
2. Push N events, for N = 0, 10k, 100k, 500k, 1M.
3. Start workers with fixed slots and offer a constant 50% of capacity on top of the backlog for 5 minutes.
4. Record, for each N: Postgres CPU-seconds per 1,000 completed events, rows read per event, engine CPU per event, and completions per second.

The pass condition for the theory is cost per event rising with N by more than the run-to-run spread (three repeats at each N). Postgres and engine CPU come from each container's cgroup `cpu.stat`, sampled every 10 s next to `pgsampler`.

This puts real load on whichever machine runs it, for about an hour in total. It can run on a Sprite instead of your Mac.

## Workload (steps 1–2)

- **Events, not runs.** Senders call Hatchet's event push (`POST /api/v1/tenants/{tenant}/events`, key `order.created`) and the worker's workflow runs on that event, so Hatchet's event tables are on the path too. One task per event, 10 ms of simulated work, a payload of about 1 KB.
- **Many sources.** 200 sources with Zipf-distributed traffic. One large source runs a flash sale: 10× its normal rate for 15 minutes, on top of everyone else. Concurrency is round-robin by `input.source` (B's tuning, unchanged).
- **Senders behave like webhook platforms.** A delivery counts once Hatchet (or Aquifer) answers 2xx within 5 s. Anything else is redelivered on a growing schedule (10 s, 30 s, 1 min, 5 min, then hourly), and the event is dropped after 19 attempts. This is what makes circuit breaking's cost visible: the backlog moves to the sender, it shows up as redelivery traffic and delay, and in the worst case events are dropped.

## Configurations

These reuse the existing `run.sh` configurations with the event workload:

| | Where the backlog lives |
|---|---|
| **B** | Hatchet's Postgres. Tuned Hatchet accepts everything. |
| **D** | The senders. Envoy in front refuses past Hatchet's capacity (circuit breaking), and senders redeliver. |
| **C** | Aquifer's disk. Aquifer accepts, then forwards at the rate Hatchet advertises from its own queue depth (Patch 2). |

## Measurements

- **Cost:** Postgres and engine CPU-seconds per 1,000 completed events, over time and against Hatchet's queue depth at that moment. This is the headline number.
- **Speed:** completed events per second during and after the sale, time to drain, and p50/p99 from first delivery attempt to completion.
- **Sender side:** delivery attempts per event, redeliveries per second, and events dropped after 19 attempts.
- **Fairness:** the small sources' p99 during the sale against their steady state, and the longest gap in which a small source completed nothing.
- **Correctness:** events lost, run twice, or never started, per 100,000 sent.

## Code needed

- Worker `WORKLOAD=events`: one workflow on event key `order.created`, a single 10 ms task, callback on completion.
- Driver `WORKLOAD=events`: 200 Zipf sources, the flash-sale phase, event push instead of run trigger, the sender redelivery schedule, and the drop counter. The ledger and reports carry over.
- A cgroup CPU sampler for local runs (Fly runs already record `cpu.json`).
- Aquifer and Envoy configs pointed at the event push path.

## Cost

Step 0 is free apart from the machine it runs on. If it shows a backlog cost, run three of each of B, D and C on Fly at the document runs' machine sizes, about an hour each. That's roughly $8, and I'll ask before starting it.
