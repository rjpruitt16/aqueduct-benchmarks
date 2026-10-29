# Plan, inventory and risks

## Layout

```
a2a-agents/
  DESIGN.md         the question, workflow, resources, configurations, scenarios, metrics, acceptance criteria
  AUDIT.md          Aquifer's A2A adapter: what it supports, the gaps, minimal proposed changes
  PLAN.md           this file
  go.mod            its own module, isolated from the other suites
  internal/
    corpus/         the issues: buggy module, tests, (stand-in only) correct and wrong fixes
    agentrpc/       A2A plumbing: serve an agent, call an agent, data parts
    work/           messages between agents, and the record of each resource call
    resource/       how an agent reaches a resource: direct, queue (Redis), aquifer (/proxy)
  cmd/
    agent/          ROLE=coordinator | research | coding | verify
    mockllm/        the stand-in model (OpenAI API, GPU-like capacity, vLLM metric names)
    dbtool/         database tool service: Postgres behind a bounded pool
    sandbox/        isolated test execution with fixed slots
    dispatcher/     configuration B's queue worker
    driver/         submits workflows over A2A; ledger; verification against the sandbox
  local/smoke.sh    one configuration end to end on one machine (Docker, Go)
  deploy/           (next) Fly and RunPod scripts for the pilot
  ana/              (next) the report over a run's summary, ledger and metrics
```

## Status

| Part | State |
|---|---|
| Design, audit | Written |
| Corpus (8 issues, 3 that need a second round) | Built, unit-tested |
| Stand-in model, database tool, sandbox | Built |
| Agents (coordinator and three specialists over A2A) | Built |
| Configurations A, B, C (resource modes) | Built |
| Driver and ledger | Built |
| Local smoke test | Running A first, then B and C; results in the README once they pass |
| Scenario injection (slowdowns, pauses, capacity changes, uncooperative clients) | Next: the services already accept `/admin` changes; the driver needs a schedule of them |
| Fly and RunPod deployment | Next |
| Analysis script | Next |

## Resource and credential inventory

| Need | Have it? | Notes |
|---|---|---|
| A vLLM endpoint | **No.** No endpoint or GPU credentials are configured on this Mac or the Sprite (`RUNPOD_API_KEY`, `HF_TOKEN`, `VLLM_URL` all unset). The August vLLM runs were set up by hand. | Not needed until the pilot |
| RunPod account access | The RunPod MCP tools answer catalog queries (prices) without auth | To create a pod: set `RUNPOD_API_KEY` from runpod.io → Settings → API Keys in the environment that runs the pilot script, or authenticate the RunPod MCP server. Don't paste it into chat or commit it. |
| A Hugging Face token | Not set | Only needed for gated models. Qwen2.5-7B-Instruct is ungated, so none is needed. |
| Fly (agents, Aquifer, services) | Yes, on the Sprite (`~/.fly-token`, expires about 2026-11-03) | Same rules as the other suites; the reaper applies |
| Docker and Go for local tests | Yes, on the Sprite | The Mac has no Docker |

## The pilot (needs approval)

**Shape:**

- **GPU:** one RunPod pod, an L40S at $1.09 an hour, running vLLM with Qwen2.5-7B-Instruct at `--max-num-seqs` set low enough to saturate.
- **Everything else on Fly** in a nearby region: agents, Aquifer, Redis and the dispatcher, the database tool with Postgres, and the sandbox.
- **Runs:** 12 runs of about 15 minutes, back to back: the three configurations × four scenarios (normal, burst, GPU saturation, mixed tenants).
- **Calibration:** one hour first, to find the arrival rate that saturates the GPU and to check the model fixes enough of the corpus.

**Cost:**

| Item | Cost |
|---|---|
| GPU, about 4 hours | about $4.50 |
| Fly machines, about 4 hours | about $2–3 |
| **Total** | **about $7–10**, capped at $15 |

The pod is created and deleted by script, and the same reaper rules apply: nothing runs unattended without a hard time limit.

## Risks and confounders

- **The model may not fix the corpus.** If a 7B model fails most issues, workflows fail for reasons unrelated to admission control. **Mitigation:** calibrate first, and report fix rates per issue. Configurations are compared on the same issues and the same model, so failures to fix affect all three alike, and outcomes are split into "failed: no passing fix" and "unfinished".
- **The stand-in model is not a GPU.** Its queueing is a model of vLLM's. **Mitigation:** the pilot uses a real GPU, and the stand-in is only for plumbing and calibration.
- **Network distance.** Fly (agents) to RunPod (GPU) adds latency, equally for every configuration. It can hide small differences in per-call overhead.
- **Configuration B depends on our implementation.** It's built carefully: tenant round-robin, idempotency, backoff on refusal, capacity-matched concurrency. A different team's queue might do better or worse. Its code is part of the complexity comparison, so its size is reported too.
- **Aquifer's `/proxy` tries the resource directly first.** Until a resource signals overload, C behaves like A. That's Aquifer's design, not a flaw in the experiment, but it means C and A look alike at low load.
- **Aquifer's per-destination limits.** On `main`, `max_concurrent` applies per tenant queue unless shared destination limits (#130, unmerged) are on. With several tenants, Aquifer can send more than a resource's capacity in total. **Mitigation:** run C with the shared-limits build if #130 has merged by then. Otherwise say so, and set per-tenant limits so the total stays at capacity.
- **Non-deterministic arrival interleaving.** Runs are repeated, and distributions are compared, not single numbers.
- **Coordinator and agents are single processes.** Their own failures aren't tested in this iteration.
