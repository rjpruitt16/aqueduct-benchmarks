# A2A agents on constrained resources

Status: design, plus a local smoke test of one full workflow. **No paid runs** without approval ([PLAN.md](PLAN.md)).

## The question

AI agent workflows depend on several constrained services at once: inference on a GPU, a database with a bounded connection pool, and sandboxed code execution with a fixed number of slots. Today, agents usually protect each one with its own retry logic, or teams build a queue and dispatcher per resource. Can one durable admission layer (Aquifer) in front of all three complete more correct workflows, with fewer wasted requests and fairer sharing between tenants, and with less code and fewer components than either?

The answer may be no. A well-built queue may match Aquifer, as the hand-built queue did in the [wild series](../envoy/RESULTS.md#wild-with-envoy-plus-what-a-team-would-build-itself). Plain retries may be good enough at the loads that matter. Either is a result worth reporting.

## Premises

- **Aquifer manages retries; it isn't the source of truth for what happened.** A workflow counts as complete only when the coordinator returns a verified artifact: the proposed fix, plus the sandbox's record that the issue's tests passed. Side effects are counted where they happen: database writes in the database, executions in the sandbox's own log.
- **Four events are recorded separately and never mixed:**
  - the A2A protocol acknowledgment (the coordinator's task exists);
  - Aquifer's acceptance of a resource call (its `201`, or the stream opening);
  - a resource call actually dispatched to the backend;
  - the workflow completing with a verified result.
- **No new Aquifer features for the benchmark.** Configuration C uses what Aquifer already has: per-destination concurrency and rate limits, account queues per tenant, idempotency keys, persistence and `/proxy`. Gaps found along the way are reported in [AUDIT.md](AUDIT.md), not built.

## The workflow: a software-maintenance team

Each task is a real, small software issue: a Python function with a known bug and a test file that fails on it. The corpus (`corpus/`) holds the issues, each with its buggy module, its tests and, for the stand-in model, a correct fix.

1. **The coordinator** (an A2A agent) receives a task from a client: issue ID and tenant.
2. **The research agent** (A2A) asks the LLM for a diagnosis, given the issue text and the failing test. *Inference.*
3. **The coding agent** (A2A):
   - reads the module and its history from the database tool service (*database read*);
   - asks the LLM for a fixed version of the function (*inference*);
   - records the proposed fix as an attempt (*database write*: a side effect, with an idempotency key).
4. **The verification agent** (A2A) runs the issue's tests against the proposed fix in the sandbox (*sandbox*).
5. **If the tests fail,** the coordinator sends the coding agent back with the test output, up to two more times. Each round costs another inference call, another database write and another sandbox run.
6. **The coordinator** returns the final artifact (fix, test log, attempts), or a failure if no fix passed.

Every workflow does real work: model calls produce code, the code is executed, and tests decide. **Correctness is checked automatically.** A workflow succeeds only if the sandbox ran the issue's tests against the returned fix and they passed. The driver checks the artifact against the sandbox's own execution record.

**Models:**

- **The stand-in model** (`cmd/mockllm`), for development, smoke tests and calibration. It is an OpenAI-compatible server that returns the corpus's known diagnosis and fix, or, for a fixed subset of issues on the first attempt, a plausible wrong fix, so the retry loop is exercised. It behaves like a GPU: a fixed number of sequences generating at a fixed tokens-per-second each, a waiting queue beyond that, and vLLM's metric names on `/metrics` (`vllm:num_requests_running`, `vllm:num_requests_waiting`, `vllm:generation_tokens_total`). Its outputs are deterministic, so runs compare exactly.
- **The real model, for the pilot:** vLLM with a 7–8B instruct model (Qwen2.5-7B-Instruct, or Llama 3.1 8B Instruct) at temperature 0 with capped output, on one RunPod GPU. The corpus is easy enough that such a model fixes most issues, and verification decides, so a wrong fix is a failed workflow, not a crash. Reasoning models are avoided: their long, variable outputs make runs hard to compare.

## The three constrained resources

Each resource has its own capacity and its own way of failing, and each is configured per scenario.

| Resource | Service | Capacity limit | Failure modes |
|---|---|---|---|
| Inference | vLLM, or the stand-in | `max_num_seqs` sequences; beyond that, requests wait inside vLLM (the stand-in: same, plus an optional `503` past a waiting limit) | Saturation (queueing), slower generation, restart |
| Database | `cmd/dbtool`: an HTTP tool service in front of Postgres | A bounded pool (`DB_POOL`, default 8 connections); requests wait for a connection up to `DB_WAIT` and then get `503` | Slow queries (injected latency), brief unavailability (Postgres paused) |
| Sandbox | `cmd/sandbox`: an HTTP execution service | `SLOTS` concurrent executions (default 4); beyond that it answers `429` with `Retry-After` | Slots exhausted, executions slowed, service restart |

How the sandbox isolates each execution:

- **Isolation:** every execution runs in a fresh jail with no network, a read-only root, a private `/tmp`, and limits on memory, CPU, processes and wall time.
- **Mechanism:** locally, a `docker run --network none --read-only --memory --cpus --pids-limit` container per execution. On Fly, nsjail inside the sandbox machine, which is itself a Firecracker VM.
- **What runs:** only the corpus's tests against model-written code. It is never treated as trusted.
- **Execution log:** the sandbox records every execution with its idempotency key, so repeated executions of the same step are counted where they happen.

**Browsers are out of the first iteration.** A headless-browser pool would be a fourth resource, added the same way later.

## Configurations

All three run the same agents, workflow, corpus, models and resources, with the same capacity per resource and the same correctness check. Only the path from an agent to a resource changes (`RESOURCE_MODE`).

| | Name | Agent → resource | What it needs |
|---|---|---|---|
| **A** | Direct with retries | Each call straight to the resource. Exponential backoff with full jitter, honoring `Retry-After`; per-call timeouts; idempotency keys on database writes and sandbox runs | Nothing beyond the services |
| **B** | Queue and dispatcher | Each call goes onto its resource's Redis queue (one list per resource and tenant). A dispatcher per resource takes jobs round-robin across tenants, keeps at most the resource's capacity in flight, honors `Retry-After`, and stores results; the agent waits for its result. Idempotency by key (`SETNX`) | Redis, a dispatcher, and the queue code |
| **C** | Aquifer | Each call through one Aquifer node's `/proxy`, with the call's idempotency key and the tenant as `user_id`. Aquifer has one upstream entry per resource with its concurrency and rate (per-tenant account queues, the default), and retries until the call succeeds | One Aquifer node and its config |

Envoy isn't a configuration. It forwards or refuses requests but doesn't hold work, so in front of these resources it would be configuration A with a proxy in the middle. It would make sense only as the edge in front of the coordinator, which isn't what this experiment tests.

**Fairness between the configurations:**

- **Same capacity:** every configuration sees the same `max_num_seqs`, `DB_POOL` and `SLOTS`.
- **Same limits:** B's dispatchers and C's Aquifer are given the same concurrency limits, equal to those capacities.
- **A's retries are a careful client's,** not a naive one's: the same backoff, jitter and `Retry-After` as the other benchmarks.
- **B gets the same tenant fairness as C** (round-robin across tenants), and the same idempotency. It is built as a careful team would, not to lose.
- **Machine budget:** Aquifer (C) and Redis plus a dispatcher (B) run on comparable machines. A gets that budget back nowhere, because it needs nothing extra; that is part of what's compared.
- **Operational complexity is measured,** not argued:
  - components to deploy;
  - lines of code an application team owns (B's queue and dispatcher, A's retry wrapper, C's Aquifer config);
  - configuration lines;
  - deployment steps.

## Scenarios

**Cooperative clients** (the driver submits tasks on a schedule and waits for results):

| | Scenario | What changes |
|---|---|---|
| S1 | Normal | Every resource has headroom |
| S2 | Burst | 5× the normal arrival rate for 2 minutes |
| S3 | GPU saturation | Arrivals past inference capacity (`max_num_seqs` low); database and sandbox have room |
| S4 | Sandbox saturation | Fewer sandbox slots (2), slower tests; inference and database have room |
| S5 | Database slowdown | Every query 10× slower for 3 minutes, then Postgres paused for 30 s |
| S6 | Mixed tenants | One tenant sends 70% of tasks, five small tenants share the rest |
| S7 | Recovery | Inference capacity halved for 4 minutes (vLLM restarted with fewer sequences; the stand-in, live), then restored |

**Uncooperative clients**, reported separately: S8. The same mixed-tenant load, but the big tenant's client resubmits a task if it has no result within 10 s, without waiting for the first, and disconnects and reconnects its streams. It checks whether duplicate submissions turn into duplicate work, in each configuration.

## Measurements

- **Workflows:**
  - completed (verified), failed (no passing fix after three rounds, or an unrecoverable error), unfinished at the end of the drain;
  - end-to-end p50, p95 and p99 from submission to the verified result, by tenant.
- **Kept apart:**
  - time to the coordinator's A2A acknowledgment;
  - time to Aquifer's acceptance of each resource call (C);
  - time to each call's dispatch.
- **Requests and retries per completed workflow:**
  - at the agents (attempts, backoff time);
  - inside the path (B's dispatcher retries; Aquifer's `aquifer_dispatch_attempts_total`);
  - at each resource (requests received).
- **Duplicate side effects:**
  - database attempt rows written more than once for the same idempotency key;
  - sandbox executions run more than once for the same key;
  - inference requests for the same step repeated.
- **Resource behavior:**
  - inference: running and waiting sequences, and tokens generated, from vLLM's `/metrics`. That's the GPU-saturation measure, and `nvidia-smi` utilization on the real GPU;
  - database: pool connections in use and waiting, query counts and latency;
  - sandbox: slots busy and queued executions.
- **Per tenant:** throughput, completion share, and Jain's index over the overload.
- **Recovery and drain:** time from a capacity change to completions resuming, and to the backlog clearing.
- **Machines:** CPU and memory of every component, from Fly's metrics as in the Hatchet runs. Database CPU per completed workflow.
- **Operational complexity,** as above.

The driver keeps an independent ledger of every task from submission, and decides completion only from a verified artifact.

## Acceptance criteria for the harness (before any paid run)

1. **The local smoke test** runs one full workflow per configuration: coordinator, three agents, all three resources, at least one retry round. Each ends in a verified artifact the driver checks against the sandbox's execution log.
2. **Every task is in the ledger** with exactly one outcome, and every resource's counters are recorded.
3. **The same task set completes in A, B and C** with the stand-in model at low load, and with identical artifacts, since the stand-in model is deterministic.
4. **Every failure scenario can be injected and is logged** with its time.

## What this can't show

- **One corpus, one small model and one region.** Results about these queueing dynamics don't carry over to every agent workload.
- **The stand-in model's GPU behavior is a model.** The pilot's real GPU decides what's real.
- **An agent's own failures** (crashes mid-workflow, lost coordinator state) aren't tested in this iteration.
