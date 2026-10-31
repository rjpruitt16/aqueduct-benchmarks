# How gateways fail in production, and what that means for Aquifer

Research note, 2026-10-29. The question: how, and under what conditions, do Envoy, other proxies, load balancers and the machines under them crash or stop forwarding correctly, and what happens to in-flight requests when they do? It is written to test Aquifer's reliability claims, not to support them.

**Method and its limits.**

- **Sources.** Vendor postmortems and status-page incident reports where they exist, then GitHub issues, security advisories and Envoy's own documentation. Secondary write-ups were used only to find a primary source.
- **What this can and can't say.** Fourteen incidents say nothing about how often gateways fail across the industry. Published postmortems favor large, public, multi-tenant edge failures, while quiet failures inside a single company's mesh are rarely written up.
- **Requests are rarely described.** Postmortems almost never describe what happened to individual in-flight requests. That column below is marked **confirmed** (the source says so), **inferred** (follows from the mechanism) or **unknown**.

## Incident evidence

| # | Incident | Failure mode | Process crashed, or stopped serving correctly? | What happened to requests | Did redundancy help? | Durable work outside the gateway? | Aquifer category |
|---|---|---|---|---|---|---|---|
| 1 | [Cloudflare, 2025-11-18](https://blog.cloudflare.com/18-november-2025-outage/), 11:20–17:06 UTC | A bad Bot Management file exceeded a hard 200-feature limit; Rust `unwrap()` panic in the FL2 proxy | Crashed per request (panic) in FL2. The legacy FL proxy kept serving but scored all bots 0 | **Confirmed:** HTTP 5xx error pages; Access logins failed. **Confirmed:** retries plus a login backlog raised dashboard latency after the fix. **Unknown:** whether any reached origins | No. The same file reached every machine every 5 minutes. Bypassing to the older proxy reduced impact | Not described | 3, and 4 (config-driven, global) |
| 2 | [Cloudflare, 2019-07-02](https://blog.cloudflare.com/details-of-the-cloudflare-outage-on-july-2-2019/), 13:42–14:09 UTC | A WAF regex with catastrophic backtracking exhausted CPU on every HTTP-serving core | Saturated, not crashed: "unable to reach the processes that serve HTTP/HTTPS traffic" | **Confirmed:** 502 error pages; about 80% of traffic lost. **Inferred:** refused at the edge, never reached origins | No. The rules deploy globally | Not described | 3, and 4 (Aquifer's own CPU-bound paths) |
| 3 | [Google Cloud, 2025-06-12](https://status.cloud.google.com/incidents/ow5i3PPK96RduMcb1SsW) | Blank policy fields hit a new, unflagged code path in Service Control (API admission); null pointer, crash loop in every region | Crashed (crash loop) | **Confirmed:** 503s on external API requests; existing streaming and IaaS resources unaffected. **Confirmed:** restarts created "a herd effect" on Spanner, with no "randomized exponential backoff" | No. Global replication carried the bad data in seconds. A "red button" disabled the path | Not described | 4 (an admission layer is exactly this kind of component); 2 for client retries |
| 4 | [Google Cloud load balancing, 2021-11-16](https://status.cloud.google.com/incidents/6PM5mNd43NbMqjCZ5REh), 09:35–10:10 PT | A latent race in configuration distribution pushed bad config to the external proxy load balancers | Running, but forwarding wrongly | **Confirmed:** 404 responses. **Inferred:** never reached backends | Rollback to the last known good configuration | Not described | 3 |
| 5 | [AWS NLB, 2025-10-20](https://aws.amazon.com/message/101925/), 05:30–09:36 PDT | Health checks failed for healthy targets during network-state propagation delays; flapping removed capacity through DNS failover | Running; capacity wrongly removed | **Confirmed:** increased connection errors, worst where the remaining capacity couldn't carry the load. **Unknown:** request fate | Made it worse: automatic failover removed healthy capacity. AWS added a "velocity control" on how much capacity one NLB can remove | Not described | 3; 4 for any health-check-driven component |
| 6 | [Azure Front Door, 2025-10-29](https://azure.status.microsoft/en-us/status/history/), about 15:45 UTC to 00:05 UTC next day | A configuration change reached edge sites through a gap in deployment safety; data-plane crashes (the preliminary review; later accounts describe a latent data-plane defect) | Crashed at edge sites | **Confirmed:** connectivity errors and timeouts for front-door-hosted services. **Unknown:** request fate | Phased rollback to the last known good configuration | Not described | 3, and 4 (config-driven) |
| 7 | [Fastly, 2021-06-08](https://www.fastly.com/blog/summary-of-june-8-outage), from 09:47 UTC | A latent bug, deployed May 12, triggered by one customer's valid configuration | Edge returned errors on 85% of the network | **Confirmed:** errors; 95% recovered within 49 minutes. **Unknown:** request fate | Disabling the configuration | Not described | 3, and 4 |
| 8 | [Slack, 2020-05-12](https://slack.engineering/a-terrible-horrible-no-good-very-bad-day-at-slack/), from 16:45 PT | HAProxy's backend state went stale because the sync program exited when it ran out of slots; at scale-down, HAProxy could reach only old, terminating backends | Running; routing to too few live backends | **Confirmed:** more 503s than normal. **Unknown:** whether any were lost or retried | No. Every HAProxy had the same stale state. Fixed by a rolling restart | Not described | 2 (clients retry); 3 |
| 9 | [Istio sidecars on EKS, Tetrate, 2025](https://tetrate.io/blog/tetrate-solves-a-critical-production-issue-on-a-cluster-serving-millions-of-users) | A node AMI update restarted hundreds of pods at once; the external CA overloaded and sidecars aborted ("Aborting proxy") | Crashed (CrashLoopBackOff) | **Confirmed:** workloads couldn't start. **Unknown:** request fate | One sibling cluster went down, weakening the mirror setup. Fixed by gradual restarts | Not described | 3 (a dependency failure); 4 for anything that restarts en masse |
| 10 | [AWS App Mesh Envoy, issue #489](https://github.com/aws/aws-app-mesh-roadmap/issues/489), Feb–Jun 2024 | Envoy's soft open-file limit fell to 1024 on EKS Fargate (a Go and containerd change); "socket(2) failed… Too many open files" | Crashed (assert) under load | **Unknown** | Not described | Not described | **4: Aquifer had the same failure** (row 13) |
| 11 | [Envoy CVE-2024-34364](https://nvd.nist.gov/vuln/detail/CVE-2024-34364) | Request mirroring buffers the mirrored response without a limit, so memory runs out | OOM crash (advisory, not an incident report) | **Inferred:** every connection on the process is reset | n/a | n/a | 4 (any unbounded buffer) |
| 12 | [Envoy CVE-2023-35942](https://osv.dev/vulnerability/CVE-2023-35942) | Use-after-free when a listener drains during a config update with gRPC access logs | Crashed (advisory) | **Inferred:** in-flight requests on that process are reset | n/a | n/a | 4 |
| 13 | **First-hand:** Aquifer `cfed721` in this repo's [survival series](../../envoy/RESULTS.md#survival-capacity-arrives-late), 2026-10-28 | Fly's open-file limit (10,240) exhausted by one held stream per waiting caller | Running, but couldn't accept connections; one run never recovered (backend idle from about 13 min) | **Confirmed:** callers couldn't reach results already waiting; 23.9% of work ran in the worst run. Fixed in aquifer#133 | No (single node) | Yes: queued jobs stayed on disk | **4: it happened to Aquifer** |
| 14 | **First-hand:** Hatchet Postgres resize and restart in this repo ([`results/nx-resize-*`](../results/)), 2026-10-29 | Postgres restarted on a resize mid-overload | Postgres restarted (10–18 s); Hatchet kept running | **Confirmed:** in every setup, Hatchet marked 5–24 documents FAILED, expired 1–38 before they started (its #4287), and left 11–29 accepted but never finished. Behind Aquifer too: Aquifer had already handed them over | Not applicable | Hatchet held them in Postgres, and lost a few anyway | 2 and 3: past the hand-off, Aquifer can't help |

What Envoy's documentation says about in-flight work, confirmed:

- **Hot restart doesn't transfer connections.** "existing connections are not transferred to the new Envoy process." The old process drains them gracefully until `--parent-shutdown-time-s`, and anything still open is closed. ([hot restart](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart))
- **Overload actions shed work.** They refuse new requests with an immediate 503 (`stop_accepting_requests`) and reset expensive HTTP/2 streams (`reset_high_memory_stream`: "Envoy will reset expensive streams to terminate them"). ([overload manager](https://www.envoyproxy.io/docs/envoy/latest/configuration/operations/overload_manager/overload_manager))
- **A 503 UC reset can be ambiguous.** It can mean a request whose connection the upstream closed in a reuse race, which Envoy's own retry policy may then resend ([envoy#14981](https://github.com/envoyproxy/envoy/issues/14981)). For a non-idempotent request that is a possible duplicate.

## Findings

1. **The big public incidents are mostly configuration or data pushes hitting a latent bug, everywhere at once** (rows 1, 3, 4, 6, 7). Redundancy didn't help, because every replica ran the same bad input. Rollback, a kill switch or a bypass fixed them. Resource exhaustion (rows 2, 10, 11, 13) and stale or wrong routing state (rows 5, 8) are the next most common. A gateway process simply crashing on one host is common in issue trackers and rare in postmortems, because redundancy usually absorbs it.
2. **What clients saw is almost always a fast error:** 5xx, 404 or connection errors. Not one source shows that a refused request had already run upstream. Request-level fate (lost, retried, duplicated) is unknown in nearly every postmortem. The one confirmed retry effect is harmful: retries and herds lengthened recovery (rows 1 and 3).
3. **Rerouting new traffic isn't recovering unfinished requests.**
   - **What rerouting covers:** health checks, redundant gateways, drains and Kubernetes restarts all decide where the *next* request goes.
   - **Where an in-flight request's state lives:** in the dead process, on the dead connection and with the client. Once a connection dies, only the client knows the request existed. Whether the upstream acted on it is unknowable from the gateway.
   - **What it takes to recover:** a client retry plus idempotency at the upstream.
   - **What drains can and can't do:** they can finish in-flight work only within the drain period, and only on a graceful stop. A SIGKILL, an OOM kill, a panic or a lost host gets no drain.
4. **Durable work outside the gateway is never mentioned in these postmortems.** The systems behind the gateways (Hatchet in row 14, payment and order systems generally) own their own durability. A front door that refuses work leaves the work with the caller, and for retrying callers that is often enough.

## What this means for Aquifer

**Aquifer manages retries. It isn't the source of truth for whether an action happened.**

- **What its job record says:** that Aquifer accepted a request and what it last heard from the backend, not whether the backend's action took effect.
- **Why it can't know:** a dispatch whose response is lost, a crash between the backend acting and Aquifer recording it, or a partition all leave Aquifer unable to tell whether the action ran. It retries, which is its job, so the backend can see the same request more than once.
- **What services still need:** their own database as the record of what they did, and idempotency on the key Aquifer forwards, so a repeat is recognized there.
- **The division of labor:** Aquifer decides when and how often to try. The service decides whether something already happened.

The four categories, applied without favoring Aquifer:

1. **Aquifer could keep useful work.** This needs a failure *downstream* of Aquifer while Aquifer stays up: the backend or its database unavailable, a partition between Aquifer and the backend, or a backend restart or resize, with callers that accept asynchronous completion. Aquifer holds jobs it has acknowledged and dispatches them when the backend returns. None of the twelve external incidents is in this category. The closest evidence is our own Hatchet runs. Even there, Hatchet is itself durable once it accepts a job, so Aquifer adds value only for work Hatchet *hadn't yet accepted*. That's what the outage runs in progress measure.
2. **Already handled by retries or durable application queues:** rows 3 and 8, and any single-gateway crash behind redundant replicas. Callers that retry until done (see [the survival series](../../envoy/RESULTS.md#callers-that-retry-until-done)) recover the work themselves. Aquifer then mainly cuts the number of requests and the tail wait, not lost work.
3. **No meaningful improvement:** rows 1, 2, 4, 5, 6, 7 and 9. These fail at the edge or load balancer *in front of* where Aquifer would sit, or in dependencies it doesn't touch. Requests never reach Aquifer.
4. **Could also hit Aquifer itself:** rows 3, 10, 11, 12 and 13, and every config-push failure. Aquifer is a process with an admission path, buffers, file descriptors, a config file and a disk. Row 13 is not hypothetical: Aquifer failed exactly as in row 10, in this repository's own benchmark.

### What the current Aquifer can't guarantee

From Aquifer's own documentation (README configuration table, API.md) and this repository's results:

- **Acknowledged but not yet on disk.** With the default `AQUIFER_PEBBLE_FLUSH_INTERVAL_MS=100`, Aquifer answers before the write reaches disk. A crash loses jobs accepted since the last completed sync: normally about 100 ms, but under heavy writes a slow sync stretches it ("seconds in the Canalis soak"). `AQUIFER_MAX_SYNC_LAG_MS` bounds new admissions but not jobs already acknowledged. Only `AQUIFER_PEBBLE_FLUSH_INTERVAL_MS=0` gives zero acknowledged-job loss, at much lower throughput. A `201` is not an fsync receipt by default.
- **One copy on one disk.** "Each instance persists to its own … volume, with no external database or coordination service." Nothing is replicated. Losing the host or the volume loses the queue: every acknowledged, unfinished job and every stored result. A redundant Envoy pair has no such state to lose.
- **Duplicates during a partition.** Cluster mode is availability-first: "two nodes with temporarily different views can both accept the same user," and "This availability-first mode does not provide a strict single-owner option." Valkey remote idempotency narrows this but is a separate, optional feature.
- **At-least-once dispatch, so duplicate side effects.** A job dispatched, executed by the backend, and not recorded before a crash is dispatched again on restart. This repository measured 77–94 repeated executions per wild run from `/proxy`'s timed-out direct try ([RESULTS.md](../../envoy/RESULTS.md)). Non-idempotent backends see duplicates.
- **In-flight streams die with the process.** A caller holding `/proxy` or a stream loses the connection on a crash, like Envoy. It can reattach by idempotency key only if the same node and disk come back.
- **The durability ends at the hand-off.** Once the backend accepts a job, its losses are the backend's (row 14).
- **Its own resource limits and config are failure modes too** (rows 10–13). Aquifer has no kill switch or bypass of its own: if it fails, the path through it fails.

## Recommended experiments

These are ranked by how well they separate the two systems' *actual* guarantees, not by how likely Aquifer is to win. All three use Hatchet (calibrated `RATE=20`), every caller retrying until done, the driver's ledger and `ana/next.py`, and the same timestamps for both arms. **None will run without approval.**

**Fair baseline for every experiment:**

- **Envoy arm.** Run two Envoy replicas behind Fly's proxy. Envoy is stateless, so redundancy is cheap and teams would run it. In the Aquifer arm, run a single node with `AQUIFER_PEBBLE_FLUSH_INTERVAL_MS` at the default 100 ms, and repeat it at 0.
- **Aquifer arm.** A two-node Aquifer cluster is the fairer redundant counterpart. It doesn't replicate jobs, so it's a variant, not the baseline.

**Measurement:**

- **When recovery starts:** the injection timestamp in `events.txt`.
- **When the work is done:** the time the last document offered before the injection completes.
- **When the infrastructure is back:** the first `200` from the front door's health check.
- **Acknowledged jobs lost:** accepted (the ledger's `accepted` is set) but never completed.
- **Duplicates:** completions per document ID above one (`duplicate_completions`), cross-checked against Hatchet's runs per `doc_id` in Postgres, because the callback undercounts.
- **Retry amplification:** producer attempts per document (`retries`, `backoff_seconds`), plus front-door-to-Hatchet requests per document (Envoy's `upstream_rq_total`; Aquifer's `aquifer_dispatch_attempts_total`).

### 1. Host disappearance: the front door's machine and its disk are gone

- **Hypothesis:** with Envoy, no acknowledged work is lost, because Envoy acknowledges nothing; callers retry through the surviving replica. With Aquifer on one node, every acknowledged job not yet handed to Hatchet is lost with the disk.
- **Null hypothesis:** the two arms lose the same number of acknowledged jobs.
- **Injection:** `fly machine destroy --force` on the Envoy replica, or on the Aquifer node, together with `fly volumes destroy` on its volume, ten minutes into the sustained overload (when Aquifer's backlog is largest).
- **Why it ranks first:** it tests the guarantee Aquifer's design implies but doesn't have, durability beyond one disk. The expected result is a clear loss for single-node Aquifer. If so, the claim must be narrowed, or replication built.

### 2. SIGKILL of the front door process, disk intact

- **Hypothesis:** Envoy loses only in-flight connections, which callers retry. Aquifer with the default flush loses up to one sync window of acknowledged jobs, plus duplicate dispatches of jobs that ran but weren't recorded. Aquifer with flush at 0 loses none, and still duplicates.
- **Null hypothesis:** no difference in acknowledged loss or duplicates between Aquifer at 100 ms, Aquifer at 0, and Envoy.
- **Injection:** `fly ssh console -C "kill -9 <pid>"` on Envoy, or on Aquifer, at the peak of the burst. Let `restart: on-failure` bring it back on the same volume. Repeat 3 times per arm, because the loss window depends on timing.
- **Why it ranks second:** it measures the acknowledged-but-unsynced window and at-least-once duplicates directly, the two places Aquifer's documentation already admits risk.

### 3. Hatchet unreachable: a partition between the front door and Hatchet's API, and a Postgres outage

- **Hypothesis:** behind Envoy, callers get errors and keep retrying through a 15-minute partition (high retry amplification, no loss as long as they retry). Behind Aquifer, callers are acknowledged, and the work completes after the partition with fewer requests and a shorter tail.
- **Null hypothesis:** with callers that retry until done, completion times and loss match, and Aquifer only reduces requests.
- **Injection:** block Aquifer's or Envoy's route to Hatchet's API on Fly's private network (a deny rule on the front door's machine, `ip6tables -A OUTPUT -d <api address> -j REJECT`) for 5 and 15 minutes, separately from the Postgres stop in `next.sh outage`.
- **Why it ranks third:** it is the case Aquifer is built for, and the one most likely to show a difference. But with Hatchet itself durable and callers that retry, it may show only a difference in requests, not in work kept. It must be read together with 1 and 2, or it will flatter Aquifer.

## Sources

Primary: [Cloudflare 2025-11-18](https://blog.cloudflare.com/18-november-2025-outage/) · [Cloudflare 2019-07-02](https://blog.cloudflare.com/details-of-the-cloudflare-outage-on-july-2-2019/) · [Google Cloud 2025-06-12](https://status.cloud.google.com/incidents/ow5i3PPK96RduMcb1SsW) · [Google Cloud 2021-11-16](https://status.cloud.google.com/incidents/6PM5mNd43NbMqjCZ5REh) · [AWS 2025-10-20](https://aws.amazon.com/message/101925/) · [Azure status history (Front Door, 2025-10-29)](https://azure.status.microsoft/en-us/status/history/) · [Fastly 2021-06-08](https://www.fastly.com/blog/summary-of-june-8-outage) · [Slack 2020-05-12](https://slack.engineering/a-terrible-horrible-no-good-very-bad-day-at-slack/) · [Tetrate, Istio on EKS](https://tetrate.io/blog/tetrate-solves-a-critical-production-issue-on-a-cluster-serving-millions-of-users) · [aws-app-mesh-roadmap#489](https://github.com/aws/aws-app-mesh-roadmap/issues/489) · [envoy#14981](https://github.com/envoyproxy/envoy/issues/14981) · [CVE-2024-34364](https://nvd.nist.gov/vuln/detail/CVE-2024-34364) · [CVE-2023-35942](https://osv.dev/vulnerability/CVE-2023-35942) · [Envoy hot restart](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart) · [Envoy overload manager](https://www.envoyproxy.io/docs/envoy/latest/configuration/operations/overload_manager/overload_manager).

Not used as evidence: secondary summaries (several give conflicting durations for rows 3 and 6), and generic troubleshooting blogs.

**Not verified directly:** Azure's final post-incident review for row 6 (only the preliminary review was available), and the full text of Google's 2021 report for row 4.
