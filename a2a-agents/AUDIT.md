# Audit: Aquifer's A2A adapter for this experiment

Source: `a2aadapter/a2a_adapter.go` in rjpruitt16/aquifer `main` (a2a-go v2.5.0, JSON-RPC binding only), and `ADAPTERS.md`. Read, not changed. The smoke test confirms the parts marked **(checked)**.

## What it is

One A2A task = one Aquifer job = one HTTP call to a resource.

- **Input:** a message whose data part is an Aquifer `JobRequest` (`user_id`, `idempotent_key`, `url`, `method`, `headers`, `body`).
- **What the adapter does:**
  - `Enqueue`, then `SubscribeJob`;
  - translates job events into A2A task events: `submitted`, `working` on enqueue and on each dispatch, an artifact with the response, then `completed` or `failed`.
- **What it isn't:** an agent orchestrator. It lets an A2A client hand Aquifer a resource call.

## Fit for this experiment

The agents could submit resource calls to Aquifer over A2A instead of HTTP `/proxy`. Configuration C uses `/proxy`, for three reasons: it's the path the other benchmarks measured, it replays results for duplicates, and it lets one node serve the agents. The A2A path is a possible variant. Five gaps decide whether it's usable:

| Topic | What the adapter does | Consequence | Minimal change (proposed, not built) |
|---|---|---|---|
| **Cancellation** | `CancelTask` returns `ErrUnsupportedOperation`. Aquifer has no job cancellation. | A coordinator that abandons a workflow can't stop its queued resource calls; they still run. | Cancellation is a core feature (remove a queued job; let an in-flight one finish), not adapter glue. File separately. |
| **Duplicate submissions** | A duplicate `idempotent_key` maps to the existing job. If that job already finished, the task ends at once with "already reached a terminal state… original response is not retained for replay". | A retried A2A submission for a finished call gets no result, so the caller can't continue the workflow from it. | The comment is out of date: Aquifer now stores results (`JobResult`, served by `GET /jobs/:id`). The adapter could return the stored result as the artifact. A small adapter change. |
| **Result after reconnecting** | Task state is in the A2A SDK's in-memory task store. If the client disconnects mid-stream, `Execute` returns when its context ends. | The job continues in Aquifer, but nothing updates the A2A task to `completed` afterwards (**to check**), and `GetTask` after an Aquifer restart finds nothing. A reconnecting client must use the job, not the task. | Keep the job ID in task metadata, answer `GetTask` from the job's stored state and result, and keep the SDK's task store on disk. |
| **Idempotency keys** | Taken only from the data part's `idempotent_key`. A2A's own `messageId` or `taskId` are not used. `user_id` falls back to the A2A tenant. | A client that retries with the same A2A message but no key creates a second job. | Document it, or fall back to `messageId` as the key when none is given. |
| **Status meaning** | `completed` = Aquifer's job completed = the resource answered. | It says nothing about whether the agent's workflow succeeded. The experiment keeps "resource call completed" apart from "workflow verified". | None: that's correct for a resource call. |
| **One front door** | `AQUIFER_ADAPTER` picks one adapter (http, a2a or mcp-stdio). | One node can't serve A2A and HTTP `/proxy` at once. | None needed here. |

## Findings to file as issues (after review, not as part of this experiment)

1. The A2A adapter could replay stored results for duplicate submissions.
2. The A2A adapter could answer `GetTask` from the job after a disconnect or restart.
3. Job cancellation (core).
