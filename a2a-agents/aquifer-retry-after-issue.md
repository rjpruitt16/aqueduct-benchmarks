# Draft issue for rjpruitt16/aquifer

**Title:** Admission Retry-After escalates to 60 s after a few seconds of disk lag

Found in the A2A agent benchmark's local smoke test (aqueduct-benchmarks,
branch `a2a-agents`), on Aquifer `main` at 25c53b5.

**What happened.** At startup on a slow disk (a Fly Sprite), WAL syncs lagged
1–3 s for about two seconds. Eight jobs arriving in that window were refused
(`last WAL sync 1083ms ago exceeds limit 1000ms`, …).
`Controller.RetryAfterSeconds` (`internal/admission/admission.go:358`) doubles
the base value for every consecutive refusal, so those eight refusals took
Retry-After from 5 s to the 60 s cap. Callers that honored it waited 40–60 s
after a stall of about 3 s.

**Effect in the smoke test** (8 workflows, tight capacity, the same work in
each configuration):

| | p50 | p99 | Client waits from Retry-After |
|---|---|---|---|
| A (direct, client retries) | 28 s | 46 s | n/a |
| B (Redis queue + dispatcher) | 37 s | 48 s | n/a |
| C (Aquifer `/proxy`) | 56 s | 76 s | 230 s in total, all from the 8 startup refusals |

Small tenants waited the same 40–60 s as the big one.

**Why it matters.** The streak counts refusals across all callers, so a burst
of simultaneous arrivals during one short stall escalates as fast as a long
outage does. That works against "don't back off, come back when Aquifer says":
callers are told to stay away far longer than the condition lasts.

**Possible directions (for discussion, nothing built):**

- escalate on how long the refusing condition has lasted, not on how many
  requests were refused;
- for sync lag, derive Retry-After from the lag itself (the lag rounded up,
  plus jitter).

The smoke test ran on a slow disk, so how often this happens on Fly volumes is
unknown. The pilot will report the number of refusals and the Retry-After
values it saw.

Related, smaller: queued dispatches use a fixed 30 s client timeout
(`dispatch.DispatchClient`), with no setting. Calls to resources that take
longer, like sandboxed test runs under contention, get cut off and retried.
