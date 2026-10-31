# Results: Envoy or Aquifer in front of an overloaded service

Fly, 2026-10-26 and 27. Backend and front door in `ord`, clients in `ord`, `iad`, `dfw`, `lhr` and `syd`, Envoy v1.39.3, Aquifer `630501d`. Raw data is in `results/`; the setup is in the [README](README.md).

| Runs | Series | Harness image |
|---|---|---|
| Steady, Envoy and Aquifer (3 each) | `env` | `registry.fly.io/aqb-envoy-images:a3b903d` |
| Changing, Envoy (2) | `env` | `registry.fly.io/aqb-envoy-images:a3b903d` |
| Changing, Aquifer (3) | `env2` | `registry.fly.io/aqb-envoy-images:c81776d` |
| Chains, Envoy and Aquifer (3 each) | `chains` | `registry.fly.io/aqb-envoy-images:cd70070` |
| Wild, Envoy and Aquifer (3 each) | `wild` | `registry.fly.io/aqb-envoy-images:504ffe1` |
| Wild, Envoy with idempotency and Envoy with a queue (3 each), Envoy and Aquifer as a check (1 each) | `diy` | `registry.fly.io/aqb-envoy-images:a7e8d73` |

**What this measures.** Work getting done under contention, with capacity that grows. It is not a fair test of instant responses: neither Envoy nor a queue answers instantly under contention, and Envoy is not designed for instant deadlines unless the service is over-provisioned. Interactive traffic belongs on a live connection (a WebSocket, a stream, or Aquifer's `/proxy`) to a backend with room to spare. The 2 s deadline is shown for completeness, not as a criterion.

**Runs left out.** Runs where the harness or setup was broken were removed (they remain in git history):

- `env` Aquifer changing: the backend sent its whole capacity as a fixed header to every queue, so Aquifer overloaded it.
- `env2` scale-out, all three setups: Envoy listed machines before they were up, and its default panic threshold then routed traffic to them; Aquifer's pool had no per-machine limit on requests in flight.
- Smoke and trial runs, and one run that failed in setup.

The scale-out comparison will be re-run once both are fixed.

## Steady: 0.5× load, capacity to spare

| | Envoy (3 runs) | Aquifer (3 runs) |
|---|---|---|
| Jobs completed | 100% (one missed its 2 s deadline, in `lhr`) | 100% |
| Requests per completed job | 1.00 (31–48 refused per run, then retried) | 1.00 (none refused) |
| Duplicated or orphaned work | 0 | 0 |
| Worst tenant p99, submit to result | 0.36–0.39 s | 0.36 s |
| Worst first-answer p99 | 0.36–0.38 s | 0.09 s |

A tie. With spare capacity neither front door costs anything. Envoy's handful of refusals were bursts above a tenant's token bucket.

## Changing: 3× spike, capacity doubles 15 minutes in

About 90,000 jobs offered per run; the backend could have done about 108,000 (40/s for 15 minutes, then 80/s). The backend sends live Aqueduct headers: its current capacity divided by the queues Aquifer reports, lower when it is overloaded. Clients retry a refused job up to 6 times, with exponential backoff (200 ms up to 20 s) and full jitter.

| | Envoy (2 runs) | Aquifer (3 runs) |
|---|---|---|
| Jobs completed | 52,800 (59%) | **59,900–60,100 (67%)** |
| Jobs given up after 6 tries | 32,300–32,400 | 29,600–30,000 |
| Jobs given up at the 2 s deadline (old rule, see below) | 4,700–4,800 | – |
| Retries per job offered | 2.4 | 2.0 |
| Requests per completed job | 5.8 | **4.5** |
| Backend errors | 0 | 0 |
| Work done for a caller that had already gone | 59–77 jobs | **0** |
| Duplicated executions | 0 | 0 |
| Small tenants' jobs completed | 84–88% | **99.9–100%** |
| Big tenant's jobs completed | 44% | 51–52% |
| Backend throughput, 8 then 16 workers (jobs/s) | 32–34, then 46–52 | 36, then 51 |
| Worst tenant p99, submit to result, completed jobs only | 7 s | 3.5–3.6 min |
| Deadline jobs answered within 2 s | 47% | 25–26% |

- **More work done, with fewer requests.** Aquifer completed about 7,200 more jobs per run (14%), with about a fifth fewer requests per completed job, and none of the backend's work was wasted on callers that had gone.
- **Fair.** Small tenants' jobs all completed, bar a handful. Under Envoy, 12–16% of theirs were given up.
- **Driven as hard as Envoy drove it.** With live headers the backend kept almost no queue of its own and ran at Envoy's throughput or better.
- **Latency.** Envoy's p99 counts only the jobs that got through; the 37,000 it turned away never finished (32,000 after six attempts, 4,800 at the 2 s deadline). Aquifer held work instead of turning it away, so its slowest completed jobs waited minutes, and it finished thousands of jobs Envoy dropped.
- **Both turned work away at the peak.** The spike offered more than the backend could do. Aquifer refuses at its front door once it holds about 60 s of the backend's capacity (the backend's `X-Aqueduct-Max-Backlog`); a longer backlog would complete more and wait longer.

Caveat: the Envoy runs come from `env`, where the client gave up on deadline jobs at 2 s, not just measured it (about 4,800 jobs per run, 5%). Some of those would have completed with more tries. Envoy changing has not been re-run under the measured deadline.

## Chains: nothing is given up, everyone retries until it works

The question here is what overload costs when no caller gives up. Every tenant runs chains of four dependent jobs and retries a refused step until it succeeds, so a refusal turns into waiting instead of a lost job. Six tenants have the same demand and differ only in region (`ord`, `lhr`, `syd`) and in how they retry (polite exponential backoff, or aggressive retries every 100 ms); a seventh, `big`, is four times their size. Load reaches 3×, and capacity doubles 20 minutes in. See the [README](README.md#regimes) for the setup.

All six runs are clean: every job completed, none were left open, and the backend returned no errors.

| | Envoy (3 runs) | Aquifer (3 runs) |
|---|---|---|
| Chains completed | 21,700–22,000 | **22,700** |
| Requests sent | 306,000–311,000 | **90,800–90,900** |
| Requests per completed job | 3.5 | **1.0** |
| Peak requests per second at the front door | 207–368 | **52–60** |
| Most tries for one job | 195–234 | **1** |
| Time clients spent backing off, all tenants | about 30 hours per run | **0** |
| Chains completed, each same-size tenant | aggressive 3,000–3,300, polite 2,000–2,200 | **2,850–2,900, all six** |
| Slowest chains (p99), same-size tenants | aggressive 14–17 s, polite 1.9–2.8 min | **9 s, all six** |
| Slowest chains (p99), `big` | 2.8–2.9 min | **36 s** |
| Backend share, each same-size tenant | aggressive 14–15%, polite 9–10% | **13%, all six** |
| Backend share, `big` | 28–29% | 24% |
| Duplicated or orphaned work | 1 duplicate in one run | 0 |

- **Under Envoy, retrying harder wins.** Tenants with the same demand did 50% more work when they retried aggressively, and their slowest chains took seconds where polite tenants' took minutes. Being polite was punished. Region barely mattered; retry style did.
- **Under Aquifer, how a tenant retries does not matter.** Each job was sent once and held until it ran. The six same-size tenants got the same share and the same latency, wherever they were.
- **The same backend did slightly more with far less traffic.** Aquifer completed about 4% more chains while the front door saw under a third of the requests and about a fifth of the peak rate. Under Envoy, roughly 70% of all requests were refused retries.
- **The big tenant.** Aquifer splits capacity evenly between queues, so `big`, with four times the demand, got 24% of the backend instead of 28% and completed about 13% fewer chains than under Envoy. Its slowest chains still finished five times sooner (36 s against nearly 3 minutes), because it never sat in backoff.

**What this says, simply.** When callers must eventually get their work done, refusing them does not reduce the work. It only adds requests and waiting, and it hands capacity to whoever retries hardest. A queue that holds the work does the same job with one request each, and shares capacity by tenant, not by how aggressive its client code is.

Round 3 note: the Fly access token on the orchestrating machine expired after both round 3 runs had finished and all clients had reported. Their results were copied and the apps destroyed by hand with the same commands `run.sh` uses, so `run.txt` for round 3 was written by hand, with the load start taken from the backend's samples.

## Wild: retries stacked across a chain of services

The question here is what happens when retries pile up the way they do in real systems. A client request goes through four services: `api` calls `orders`, which calls `inventory` and `payments` in parallel, and those two call the front door (Envoy or Aquifer) in front of the backend. Every service tries 3 times (50 ms, then 100 ms apart, no jitter) before passing a 503 up, so one client try can become up to 27 calls at the bottom. Each service has its own timeout: 10 s, 8 s, and 5 s at the bottom. The request ID travels down the chain and stays the same when a client resends.

There are 15 tenants in three regions (`ord`, `lhr`, `syd`). Each region has one large tenant (40 workers, 3 requests/s at 1×) and four small ones (10 workers, 1 request/s), one for each retry style:

- polite: exponential backoff with full jitter
- no-jitter: honours `Retry-After` without adding randomness
- aggressive: retries every 100 ms
- stacked: a library's 3 tries inside an SDK's 3 tries inside the app's polite loop

The large tenants use polite, no-jitter and stacked retries. Each worker sends one request at a time and retries it until it succeeds, giving up a try after 15 s. Load runs for 40 minutes with a 3× peak and seven bursts of at least 4×. The backend refuses everything for 60 s at 20 minutes, with nothing in its headers to announce it, and its capacity doubles at 25 minutes. See [DESIGN-wild.md](DESIGN-wild.md) for the full setup.

All six runs are clean: every request completed and none were left open. The three rounds agree closely.

| | Envoy (3 runs) | Aquifer (3 runs) |
|---|---|---|
| Client requests completed | 20,500–22,200 | **46,200–46,300** |
| Completed per second, whole run | 8.5–9.2 | **19.2** |
| Client sends per completed request | 4.1–4.5 | **1.15–1.16** |
| Requests reaching the front door | 3.7–3.9 million | **197,000** |
| Backend executions | 97,900–98,800 | 92,400–92,700 |
| …of which repeats of work already done | 53,500–57,800 (55–58%) | **77–94 (0.1%)** |
| Backend refusals | 8,500–8,800 | **510–660** |
| Peak client sends per second | 87–89 | **44–48** |
| Time clients spent backing off, all tenants | 49–54 hours per run | **0.5 hours** |
| Client tries that hit the 15 s timeout | 0–57 | 7,100–7,500 |
| Completed, each small tenant | 850–1,480 | **2,490–2,680** |
| Slowest requests (p99), small tenants | 1.3–4.2 min | **9–11 s** |
| Completed, each large tenant | 1,750–2,330 | **5,010–5,140** |
| Typical request (p50), large tenants | **0.3–1.4 s** | 3.0–4.3 s |
| Slowest requests (p99), large tenants | 7–15 min | **35 s** |

- **The backend did about the same amount of work, but under Envoy more than half of it was wasted.** It ran about 92,000–99,000 jobs in both setups. Under Envoy, 55–58% were repeats: a bottom service timed out or a service above it retried, and the job ran again. Under Aquifer a resend carries the same key and joins the job already queued, so almost nothing ran twice. About 42% useful work against 99.8% is the 2.2× gap in completed requests.
- **Each hop multiplied the traffic under Envoy.** 90,000 client sends became about 240,000 calls to `orders`, 1.3 million from `orders` to the bottom services, and 3.7–3.9 million at the front door. Most of those were cheap 429s from Envoy's own rate limits, so the front-door count overstates the cost; the repeated backend work is the cost that matters. Under Aquifer the same chain sent about 197,000 requests to the front door, 2.1 for each backend job needed.
- **Under Envoy, how a tenant retries changed what it got; under Aquifer it did not.** Small tenants with the same demand completed 850–1,480 requests under Envoy. Aggressive retries sent 5.1–6.6 requests per completion and still completed the least in every run, probably because their own fast retries used up their tenant's rate limit and caused more repeats below; stacked retries sent 4.2–5.2. Under Aquifer all twelve small tenants completed 2,490–2,680 requests at 1.02 sends each, whatever their style or region.
- **Recovery from the outage.** Before the outage, Aquifer was completing about 17 requests/s and Envoy 3–6. In the first minute after the backend came back, Aquifer was at about 14/s and was back to 17/s the minute after. Envoy returned to its pre-outage rate straight away, but that rate was already low: its clients were deep in backoff and the chain was full of retries. Envoy only climbed (to about 21/s) when capacity doubled at 25 minutes. The outage cost each Aquifer worker about 4 timed-out tries (60 s ÷ 15 s), which is where the small tenants' 40 timeouts each come from.

Where Aquifer was worse, or the comparison is limited:

- **Large tenants wait longer.** Aquifer shares capacity evenly between tenants, so the three large ones queued for longer. Their typical request took 3–4 s against Envoy's 0.3–1.4 s, and about 30% of their tries hit the 15 s client timeout. Those resends joined the job already waiting, so they cost requests but almost no backend work, and the large tenants still completed more than twice as many requests as under Envoy.
- **Aquifer repeated some work.** 77–94 jobs per run ran twice and the second result was thrown away. This is probably caused by the 3 s timeout on `/proxy`'s first direct try, after which the job is queued while the first try may still be running.
- **The 5 s timeout on the bottom services is strict for Aquifer.** A queued job can take longer than that, and each re-wait counts as a request. A team using Aquifer would probably wait longer or take a webhook; the test keeps 5 s for both.
- **Idempotency is a large part of this.** Envoy is a stateless proxy and cannot tell a resend from a new request. Here the services have no deduplication of their own, which is the worst case for a proxy. A team could add idempotency keys and a durable fair queue in front of the backend; the next section measures that.
- **The backend is synthetic.** It has a fixed service time, no database and no shared state, all services are identical, and the outage is a clean refusal rather than a slow or partial failure.

**What this says, simply.** When every layer retries, a refusal at the bottom is multiplied on the way up and comes back down as repeated work. The same backend then spends most of its time redoing jobs it already finished. Holding the work and recognising a resend by its key let the same backend complete about twice as many requests, with about a quarter of the client sends per request and the same treatment for every small tenant. The cost was paid by the largest tenants, which waited longer for an even share.

## Wild, with Envoy plus what a team would build itself

The wild comparison put Envoy in as a bare proxy, with no deduplication in the services. A team that saw half its backend work repeated would not leave it there. This series keeps Envoy and the wild regime exactly as they were and adds the two things such a team would most likely build, using the same job ID Aquifer keys on and the same live `X-Aqueduct-Max-Concurrent` header Aquifer follows:

- **Envoy with idempotency keys** (`envoy-idem`). `inventory` and `payments` check Redis for a stored result before calling Envoy, and take a lock so that only one copy of a job runs; a repeat waits for that copy's result. A refused job is still refused and retried from above.
- **Envoy with a fair queue** (`envoy-queue`). The same check, but a job that isn't done goes onto its tenant's list in Redis, once. A dispatcher takes jobs round-robin across tenants, keeps as many in flight as the backend advertises, waits out `Retry-After` on a refusal, and wakes the waiting callers when the job finishes.

Both new setups ran three rounds. Round 1 also ran plain Envoy and Aquifer as a check: Envoy completed 22,600 requests (wild: 20,500–22,200) and Aquifer 46,400 (wild: 46,200–46,300), so the table compares the new setups with all of wild's rounds. See [DESIGN-diy.md](DESIGN-diy.md) for the design. All twelve runs are clean: every request completed and none were left open.

| | Envoy (wild, 3 runs) | Envoy + idempotency (3) | Envoy + queue (3) | Aquifer (wild, 3) |
|---|---|---|---|---|
| Client requests completed | 20,500–22,200 | 44,700–45,200 | **47,200–47,500** | 46,200–46,300 |
| Completed per second, whole run | 8.5–9.2 | 18.6–18.8 | **19.6–19.7** | 19.2 |
| Client sends per completed request | 4.1–4.5 | 1.70–1.75 | **1.15** | **1.15–1.16** |
| Requests reaching the front door | 3.7–3.9 million | 1.5–1.6 million | **95,000–96,000** | 197,000 |
| Backend executions | 97,900–98,800 | 89,500–90,500 | 94,400–95,000 | 92,400–92,700 |
| …of which repeats of work already done | 53,500–57,800 | **0** | **0** | 77–94 |
| Backend refusals | 8,500–8,800 | 8,300–8,400 | **540–590** | 510–660 |
| Peak client sends per second | 87–89 | 80–87 | **38–48** | 44–48 |
| Time clients spent backing off | 49–54 hours | 11.5–14 hours | 0.6 hours | **0.5 hours** |
| Client tries that hit the 15 s timeout | 0–57 | **0** | 7,000 | 7,100–7,500 |
| Completed, each small tenant | 850–1,480 | 2,200–2,460 | **2,550–2,720** | 2,490–2,680 |
| Slowest requests (p99), small tenants | 1.3–4.2 min | 34–45 s | **9–11 s** | **9–11 s** |
| Completed, each large tenant | 1,750–2,330 | **5,360–6,430** | 5,260–5,360 | 5,010–5,140 |
| Typical request (p50), large tenants | 0.3–1.4 s | **0.4–1.4 s** | 3.3–4.2 s | 3.0–4.3 s |
| Slowest requests (p99), large tenants | 7–15 min | 1–3 min | **31 s** | 35 s |
| Extra machines | none | Redis | Redis, dispatcher | none |
| Redis commands per run | | 4.8–5.0 million | 1.15 million | |

- **Idempotency alone recovered most of the gap.** With a result store and a lock in the bottom services, no job ran twice and the services completed 97% as many requests as Aquifer. This is more than the design predicted. The storm did not go away: each refusal still went back up the chain, so clients sent 1.7 requests per completion, the front door still saw 1.5–1.6 million requests (mostly Envoy's 429s), and clients spent 11–14 hours backing off per run. It also kept Envoy's shape on latency: large tenants got more than their even share and the fastest typical requests of any setup, while small tenants waited longest at the tail (p99 34–45 s).
- **Idempotency plus a fair queue matched Aquifer and did slightly better.** It completed about 2% more requests, sent half as many requests to the front door (one per job, because the services never call it directly), and its latencies and per-tenant shares were close to Aquifer's in every row. Small tenants completed the same whatever their retry style. Like Aquifer, it pays for holding work with client timeouts on the large tenants: about 7,000 per run, almost all of them re-waits on a job already queued.
- **Before the outage, the hand-built queue ran faster than Aquifer.** In minutes 17–19, with the backend allowing 8 in flight, it completed about 19.3 requests/s against Aquifer's 17.1 in the same round. After capacity doubled both ran at 30–33/s. Aquifer seems to leave some of the advertised concurrency unused at the lower limit; that is worth looking into in Aquifer.
- **Recovery from the outage.** In the first minute after the backend came back, the queue completed about 10 requests/s, Aquifer about 14 and Envoy with idempotency about 13; all three were back at their earlier rate the minute after. During and after the outage the queue held about 450 jobs, and it emptied within a minute of capacity doubling.

What it costs to do it yourself:

- **Machines.** Both setups add a Redis instance (shared-cpu-2x, 1 GB, append-only persistence); the queue also adds a dispatcher of the same size. Aquifer is one machine with its own store. Redis was lightly loaded with the queue (1.15 million commands, 52–66 s of CPU and 25 MB over a run) and busier with idempotency alone (4.8–5.0 million commands, about 110 per completed request, and 120–141 s of CPU), because every retry in the storm checks the store.
- **Code.** About 190 lines added to the services for both modes and a 311-line dispatcher, both in Go with `go-redis`, and both counting the benchmark's own reporting. That is code a team has to own: the lock's expiry, a lost wake-up, a job requeued after a crash, a dispatcher that is a single point of failure (one here, as designed). None of those failure cases were tested; nothing restarted during these runs.

Limits of this comparison:

- **Built for this test, by someone who knew the answer.** The DIY setups were written knowing exactly what the wild regime does, with the job ID and the concurrency header already available. A team starting from a real system would have neither for free.
- **One dispatcher, one Redis, no failures.** The queue has no redundancy, and the runs did not test a Redis or dispatcher restart. Aquifer's own durability and failover were not tested here either.
- **The same synthetic backend and 5 s timeout** as wild, with the same caveats.

**What this says, simply.** Most of Aquifer's advantage in the wild regime is something a team can build: idempotency keys stop the repeated work, and a fair queue that follows the backend's limit stops the storm. Built that way, Envoy plus Redis plus a dispatcher matched Aquifer here and was slightly faster before the outage. The difference is what the team owns: two more machines and about 500 lines of code, against one service that does the same thing out of the box. Neither side's failure handling was tested here: no Redis, dispatcher or Aquifer restart happened in any run, so which one holds up better when something breaks is still open.

## What we can say

- When the service reports its load and Aquifer follows it, Aquifer completes more work than Envoy, with fewer requests, no wasted backend work and complete fairness to small tenants. Individual jobs wait longer, because it holds work instead of refusing it.
- When callers retry until their work is done, Envoy's refusals turn into retries: three and a half requests per job, with capacity going to the tenants that retry hardest. Aquifer did slightly more work with one request per job and gave same-size tenants the same share and latency, whatever their region or retry style. The cost is that a much bigger tenant gets an even split rather than the larger share it can grab by volume.
- When retries stack up across a chain of services, Envoy's refusals multiply at every hop and the backend spends more than half its time redoing work. Aquifer recognises a resend by its key and runs the job once, so the same backend completed about 2.2 times as many requests; the largest tenants paid for the even split with longer waits. That was Envoy as a bare proxy. With idempotency keys in Redis the services completed 97% as many requests as Aquifer with no repeated work, but the retry storm stayed. With a fair queue and a dispatcher as well, they matched Aquifer and did slightly better, at the cost of Redis, a dispatcher and about 500 lines of code to own.
- A rate limit alone does not protect a service that slows down under load; it also needs a limit on requests in flight. Aquifer gets one from the backend's `X-Aqueduct-Max-Concurrent`, but its pools do not have one yet.

Next: find out why Aquifer left some of the advertised concurrency unused at the lower limit in the do-it-yourself series, a per-member concurrency limit for Aquifer pools (needs a design), then the scale-out regime with that and `healthy_panic_threshold: 0` for Envoy, and Envoy changing under the measured deadline.
