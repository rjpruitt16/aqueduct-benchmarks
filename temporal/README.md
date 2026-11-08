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

## Follow-up: a caller that keeps retrying, and workers

Same burst (600/s for 3 minutes), same 200/s namespace. "Retrying" means the caller retries after the SDK gives up, with jittered backoff up to 30 s, for up to `RETRY_FOR` after the offer. The worker (`cmd/worker`) runs each workflow with one no-op activity: about 3 frontend calls per workflow, plus polls, all under the same namespace limit.

| | A: SDK + retrying caller, no worker | C: SDK + retrying caller, worker | D: Aquifer at 190/s, worker |
|---|---|---|---|
| Started | 108,000 (all) | 108,000 (all) | 108,000 (all) |
| Lost | 0 | 0 | 0 |
| Calls to Temporal per start | 5.37 (448,351 refused) | 5.24 (428,224 refused) | **1.0** (0 refused) |
| Start latency p50 / p99 | 180 s / 482 s | 183 s / 517 s | 198 s / 560 s |
| Workflows the worker finished while starts were draining (~12 min) | – | **77** | **1,321** |

Reading it:

- **A caller that retries long enough loses nothing either.** The 44% loss in the first table is the SDK's default give-up, not something only Aquifer avoids. Aquifer's remaining edge on starts is one call each instead of about five, and holding the overflow durably instead of in the callers' memory.
- **Workers starve under any start burst that fills the namespace limit.** Starts and the worker's own calls share the 200/s. With retrying callers the worker finished 77 workflows in 12 minutes; behind Aquifer it finished 1,321, about 17 times more but still under 2 a second, because Aquifer was configured at 190/s and left the worker about 10/s. In both, about 105,000 started workflows were still waiting to run when the starts finished.
- So the useful setting is not "pace just under the limit" but "pace starts at the limit minus what the workers need". Aquifer can do that from config or from a rate Temporal advertises; Temporal doesn't advertise one, so it's config. A caller-side token bucket could do the same, without durability.

## Follow-up: the same limit on both sides (120/s)

Is Aquifer doing more than rate limiting? Both sides now get the same 120/s, leaving about 80/s for the worker. E is the retrying caller behind a client-side limit shared by all 2,000 callers in its process, its own retries included. F is Aquifer at 120/s. G is F with Aquifer killed (`kill -9`) 90 s into the burst and restarted 5 s later. Counts in the last row come from Temporal itself (`temporal workflow count`), not from the harness.

| | E: retrying caller, limit 120/s | F: Aquifer 120/s | G: Aquifer 120/s, killed at 90 s |
|---|---|---|---|
| Calls to Temporal per start | 1.0001 (10 caller retries, 0 refused) | 1.0 | 1.0 |
| Start latency p50 / p99 | 360 s / 716 s | 360 s / 776 s | 365 s / 851 s |
| Workflows finished by the worker at 5 / 10 / 15 min | 4,974 / 9,727 / 11,008 | 4,979 / 8,242 / 8,531 | 5,118 / 7,403 / 7,627 |
| Starts held in memory | peak 86,391 (at the end of the burst); 43,191 at 90 s | 0 (on disk) | 0 (on disk) |
| Workflows in Temporal at the end | 108,000 of 108,000 | 108,000 of 108,000 | 107,994 of 107,994 offered |

Reading it:

- **A client-side limiter matches Aquifer on scheduling.** Same start rate, same latency, and one call per start on both sides. The earlier 77 vs 1,321 was pacing versus no pacing, not anything particular to Aquifer. The worker finished slightly more under E after 5 minutes; these runs share one Mac's CPU with Aquifer and its database, so treat that gap as noise-sized, not a finding.
- **The difference is where the overflow lives.** E held up to 86,391 starts in the callers' memory; a crash of that process at 90 s would have lost 43,191. Aquifer killed at the same moment lost none: it recovered 43,180 queued jobs from disk on restart and every offered start reached Temporal. Callers retried their submissions during the 5 s it was down (120,924 intake attempts for 107,994 jobs), and none gave up.
- So Aquifer's advantage in front of Temporal is durable, central admission: one place that holds overflow across crashes and enforces one limit for every service that starts workflows. For a single application with one Temporal client, a limiter plus retries does the scheduling job. Not tested here: several independent services sharing one namespace, each with its own limiter.

(G's `temporal_attempts_by_status` reads 97,198 because Aquifer's metrics restart at zero after the kill; Temporal's own count is the one to trust. G offered 6 fewer starts than E and F because of driver timing, not loss.)

Run: `temporal server start-dev --headless --http-port 7243 --dynamic-config-value frontend.namespaceRPS=200 --dynamic-config-value frontend.globalNamespaceRPS=200`, then `RATE=600 DURATION=3m go run ./cmd/burst` (SDK), or with `AQUIFER_URL=http://localhost:8085` (Aquifer). Add `RETRY_FOR=15m` for a retrying caller, `LIMIT=120` for a client-side limit, and run `go run ./cmd/worker` alongside for workers.
