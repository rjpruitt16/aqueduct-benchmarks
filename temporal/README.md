# Temporal: workflow starts under a burst

Local check, 2026-11-07: does Temporal's front door lose work under a burst, and does Aquifer in front do better than the Go SDK's own retries? Temporal dev server 1.32.2 (CLI 1.10.0) on a Mac, namespace limited to 200 requests/s (`frontend.namespaceRPS=200`, standing in for a provisioned Temporal Cloud limit). Workflow starts only; no worker ran them.

Offered load: 600 starts/s for 3 minutes (108,000), open loop, from 2,000 concurrent callers.

| | Go SDK, default retries | Through Aquifer (paced at 190/s) |
|---|---|---|
| Started | 60,839 | 106,000 of 106,000 new (all) |
| Failed (lost unless the caller backfills) | **47,161 (44%)**, SDK gave up (deadline exceeded) | **0** |
| Calls to Temporal per start | 3.85 (415,715 calls, 354,873 `ResourceExhausted`) | **1.0** (0 refused) |
| Refused at intake | – | 0 |
| Start latency p50 / p99 | 59 s / 122 s (successes only) | 191 s / 535 s (everything) |

Aquifer ran from main (`e18da35`) with `localhost:7243: {rps: 190, max_concurrent: 8}`, 20 tenants, `max_retries: -1`, and `AQUIFER_MAX_PENDING_PER_UPSTREAM=150000` (200/s × about 12 minutes, its documented sizing). Starts went through Temporal's HTTP API (`POST /api/v1/namespaces/default/workflows/{id}`).

Reading it:

- Neither can start more than the limit allows. The difference is what happens to the overflow: the SDK alone lost 44% of it after hammering the frontend with nearly 4 calls per start; Aquifer accepted all of it durably and fed it to Temporal at the limit, so every start landed, with one call each.
- Aquifer's latency is longer because it includes the starts the SDK lost: the last of the burst started about 9 minutes after it was offered. That is the trade: late but certain, instead of on time or never.
- In the Aquifer run 2,000 of the 108,000 offered reused IDs from a smoke run just before it (a harness bug, since fixed: IDs are now unique per run). Aquifer and Temporal both treated them as duplicates, so they're left out of the counts above; Temporal holds exactly 2,000 + 106,000 such workflows.

Not yet measured: workers running the workflows (their calls count toward the same limit), more tenants and fairness between them, a real cluster, and how the SDK compares with a caller that retries for longer.

Run: `temporal server start-dev --headless --http-port 7243 --dynamic-config-value frontend.namespaceRPS=200 --dynamic-config-value frontend.globalNamespaceRPS=200`, then `RATE=600 DURATION=3m go run ./cmd/burst` (SDK), or with `AQUIFER_URL=http://localhost:8085` (Aquifer).
