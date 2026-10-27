# Design: how long does it survive?

Status: proposal, not built. Needs sign-off before any code or spend.

## Why

Every regime so far has a backend that never breaks: past its capacity it refuses or slows down, and capacity arrives on a fixed clock. Real services fall over. A service under too much load runs out of memory or threads and crashes; while it restarts its share of the fleet lands on the others; an autoscaler sees the load late and brings help a minute or two after that. The question a team actually asks is whether the system lasts long enough for help to arrive, and how much work is lost on the way.

It answers four questions:

1. Under a load that keeps rising, how long until the backend first crashes, and how often does it crash after that?
2. Does the system survive until the autoscaler's new capacity arrives, or does it keep crashing even then?
3. How much work is lost (accepted and never finished), and how long does the backlog take to drain once load falls?
4. Where does the front door itself break? For Aquifer: its backlog limit, memory and disk. For Envoy plus a queue: Redis and the dispatcher.

## Setups

Three front doors, each over the same fragile backend:

| Setup | Why it is here |
|---|---|
| `envoy` | the bare proxy, as in every regime |
| `envoy-queue` | the do-it-yourself queue that matched Aquifer in the `diy` series; the strongest baseline |
| `aquifer` | Aquifer `630501d`, as before |

`envoy-idem` is left out: it behaved like `envoy` under refusals and adds cost without a new question.

## The fragile backend

`cmd/backend` gains a crash model, off unless `CRASH` is set:

- **When it crashes.** If requests waiting inside the backend stay above `CRASH_AT` (default 3× its workers) for `CRASH_AFTER` (default 5 s), it crashes. This stands in for a service that queues internally until memory or threads run out. A front door that keeps the backend at its advertised limit never trips it; one that lets requests pile up does.
- **What a crash does.** Every request in the backend fails at once with a dropped connection (counted as lost work: the job may or may not have run, which is the honest worst case), and for `RESTART` (default 45 s, drawn between 30 and 60 s) new connections are refused, not answered with a 503. Then it starts with an empty line.
- The crash is simulated in the process, not a real machine restart, so the timing is the same in every run and the backend can keep its counts.

## Load and the autoscaler

- **Load ramps.** 15 tenants and services as in wild. Load starts at 0.5× and rises by 0.5× every 2 minutes to 6× at 22 minutes, holds for 8 minutes, then falls to 0.5× and stays there until 50 minutes, so the drain can be measured.
- **The autoscaler** is a small controller (new, `cmd/autoscaler`) that reads the backend's own report of how busy it is (time its workers spent busy over the last 30 s), the same for every setup. When that stays above 80% for 60 s it asks for more capacity; the capacity arrives `SCALE_DELAY` later (default 90 s, a cold start), in steps of +8 workers, up to 4 steps (8 → 40 workers). It never scales down during the run.
- Capacity is added inside the one backend through its existing capacity change, rather than as new machines. The scale-out comparison is still blocked on Envoy's panic threshold and a per-member limit in Aquifer's pools; this regime should not wait on that, and it isolates survival from load balancing. Real machines are a later step once the scale-out fixes land.

## Where the front door breaks

A separate, shorter run per front door, with no crash model and plenty of backend capacity held back: load ramps until the front door, not the backend, is the limit.

- Aquifer on a deliberately small machine (shared-cpu-1x, 512 MB), with its default `Max-Backlog`. Recorded every 10 s: memory, disk used by its store, backlog length, what it answers when the backlog is full, and whether it stays up.
- Envoy plus queue with Redis and the dispatcher on the same small size: Redis memory, list lengths, dispatcher memory.
- Plain Envoy on the same size, for completeness; it holds nothing, so it should only run out of connections or CPU.

## What is measured

| Measure | Why |
|---|---|
| Time to first crash, number of crashes, total time down | how long it holds and how often it falls |
| Crashes after the autoscaler's last step | whether it survived until help arrived |
| Lost work: requests in the backend when it crashed | the cost of each crash |
| Completed requests, sends per completion, repeats | as in every regime |
| Time to drain after load falls | how fast it recovers |
| Peak backlog, memory and disk in the front door (and Redis) | what holding the work costs, and where it breaks |
| Load (×) at which the front door itself first fails, and how | the breaking-point runs |

## Fairness

- The same backend, crash model, load and autoscaler for every setup; the autoscaler reads the backend, not the front door, so no setup gets an earlier signal.
- `envoy-queue` and Aquifer both follow `X-Aqueduct-Max-Concurrent`; Envoy keeps its rate limits from every series. If Envoy crashes the backend repeatedly, that is reported as what a bare proxy does, and a run with Envoy's circuit breaker (`max_pending_requests`, `max_requests`) set to the backend's limit is added so Envoy is shown configured well, not only by default.
- Lost work counts against every setup the same way.
- Where Aquifer breaks is reported as plainly as where the others do.

## Build

- `cmd/backend`: the crash model (`CRASH`, `CRASH_AT`, `CRASH_AFTER`, `RESTART`), busy-time reporting, crash and restart events.
- `cmd/autoscaler` (new, small): reads the backend's report, applies the delay and steps, logs each decision.
- `cmd/client`: the ramp schedule (a new load profile).
- `deploy/run.sh`: a `survival` regime and a `breaking` regime; the autoscaler machine; machine size overrides for the front door.
- Envoy: an extra config with circuit-breaker limits for the `envoy-cb` variant.
- Analysis: crash timeline, lost work, drain time, front-door resources.
- Local check on the Mac first, tiny, as before; then one 10-minute smoke run per setup on Fly; then the series.

## Cost

| Step | Estimate |
|---|---|
| Smoke runs (4 setups, 10 min) | about $1 |
| Survival series: `envoy`, `envoy-cb`, `envoy-queue`, `aquifer`, 50 min, 2 rounds each | about $10–12 |
| Breaking-point runs: 3 front doors, about 20 min, 2 rounds each | about $2–3 |
| Total | about $13–16 |

Two rounds instead of three: the last four series agreed closely between rounds. A third round can be added for any setup whose two rounds disagree.

## To decide

1. **Crash model:** in-process simulation as above, or real machine restarts (more realistic, noisier, harder to count lost work)?
2. **Envoy with a circuit breaker:** include `envoy-cb` (recommended, it is the fair Envoy here), or keep Envoy as configured in every series?
3. **Autoscaling inside one backend now**, real machines after the scale-out fixes (recommended), or wait and do machines only?
4. **Breaking-point runs:** include them now, or run survival first?
