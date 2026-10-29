"""Report one gateway-failures run (local smoke or Fly) in the results template's terms.

Reads summary.json (the driver's ledger), events.txt (what was injected and
when), scrape.jsonl (gateway readiness and counters sampled every 2 s) and,
when present, duplicates.json (Hatchet's own runs per document).

Every latency here is from a document's first offer to its workflow's
completion (summary.json p*_ms_by_phase) unless named "acceptance". Counters
that reset when a process restarts are summed across the restart.

Usage: python3 -I gateway-failures/ana/gateway.py <run dir> [...]   (the dir holding summary.json)
"""
import json, os, sys
from datetime import datetime

TENANTS = ["law-firm", "accounting", "small-1", "small-2", "small-3", "small-4", "small-5"]


def ts(s):
    s = s.rstrip("Z")
    if "." in s:
        head, frac = s.split(".")
        s = head + "." + frac[:6]
    return datetime.fromisoformat(s + "+00:00").timestamp()


def read_json(path, default=None):
    try:
        return json.load(open(path))
    except (OSError, ValueError):
        return default


def counters(records, name):
    """Totals of each counter line in a scrape target, summed across process restarts."""
    last, total = {}, {}
    for r in records:
        if r["name"] != name or r.get("status") != 200:
            continue
        for line in r.get("lines") or []:
            if line.startswith("#"):
                continue
            # Envoy: "name: value"; Prometheus: "name{labels} value".
            key, _, value = line.rpartition(":") if ": " in line else line.rpartition(" ")
            try:
                v = float(value)
            except ValueError:
                continue
            key = key.strip()
            prev = last.get(key)
            if prev is None:
                total[key] = v
            elif v >= prev:
                total[key] = total.get(key, 0) + v - prev
            else:  # reset: the process restarted
                total[key] = total.get(key, 0) + v
            last[key] = v
    return total


def downtime(records, name, after):
    """For a readiness target: when it first failed after `after`, and when it was next ready."""
    down = up = None
    for r in records:
        t = ts(r["at"])
        if r["name"] != name or t < after:
            continue
        ok = r.get("status") == 200
        if down is None and not ok:
            down = t
        elif down is not None and ok:
            up = t
            break
    return down, up


def pct(v, n):
    return f"{100 * v / n:.2f}%" if n else "-"


for run in sys.argv[1:]:
    s = read_json(os.path.join(run, "summary.json"))
    if not s:
        print(f"== {run}: no summary.json")
        continue
    info = {}
    for path in (os.path.join(run, "run.txt"), os.path.join(run, "..", "run.txt")):
        if os.path.exists(path):
            info = dict(l.strip().split("=", 1) for l in open(path) if "=" in l and " " not in l.split("=", 1)[0])
    events = [l.rstrip("\n") for l in open(os.path.join(run, "events.txt"))] if os.path.exists(os.path.join(run, "events.txt")) else []
    scrape = []
    if os.path.exists(os.path.join(run, "scrape.jsonl")):
        scrape = [json.loads(l) for l in open(os.path.join(run, "scrape.jsonl")) if l.strip()]
    tenants = s["tenants"]
    offered = sum(sum(t["offered_by_phase"].values()) for t in tenants.values())
    completed = s["totals"].get("completed", 0)
    print(f"== {run}  ({info.get('config', '?')})")
    print(f"  offered {offered}  workflows completed {completed} ({pct(completed, offered)})")
    acc = s.get("acceptance", {})
    print(f"  not completed, by how far they got: acknowledged by Aquifer but never reached Hatchet {acc.get('acked_not_triggered', '?')}, "
          f"accepted by Hatchet but never completed {acc.get('triggered_not_completed', '?')}, never accepted {acc.get('never_accepted', '?')}")
    outcomes = {}
    for t in tenants.values():
        for k, n in t["outcomes"].items():
            outcomes[k] = outcomes.get(k, 0) + n
    print(f"  outcomes {outcomes}  duplicate completions (callbacks) {s.get('duplicate_completions', 0)}")
    dups = read_json(os.path.join(run, "duplicates.json"))
    if dups is not None:
        print(f"  Hatchet runs per document: {dups.get('documents_with_more_than_one_run', '?')} documents ran more than once, "
              f"{dups.get('extra_runs', '?')} extra runs")
    else:
        print("  Hatchet runs per document: not measured in this run")

    print("  per tenant, overload phase (completion = first offer to workflow completion):")
    for name in TENANTS:
        v = tenants.get(name)
        if not v:
            continue
        done = v["outcomes"].get("completed", 0)
        print(f"    {name:10} completed {done:6d}  completion p50/p95/p99 "
              f"{v['p50_ms_by_phase'].get('overload', 0) / 1000:7.1f}/{v.get('p95_ms_by_phase', {}).get('overload', 0) / 1000:7.1f}/{v['p99_ms_by_phase'].get('overload', 0) / 1000:7.1f} s"
              f" | first acceptance p50/p99 {v.get('accept_p50_ms_by_phase', {}).get('overload', 0) / 1000:5.1f}/{v.get('accept_p99_ms_by_phase', {}).get('overload', 0) / 1000:5.1f} s"
              f" | retries {v.get('retries', '?')} on {v.get('retried_documents', '?')} docs, backoff {v.get('backoff_seconds', 0):.0f} s")
    print(f"  fairness (Jain, overload) {s.get('jain_overload', 0):.4f}")

    first_event = ts(events[0].split(" ")[0]) if events else None
    if events:
        print("  events: " + "; ".join(e.split(" ", 2)[1] + " " + e.split(" ", 2)[2] for e in events))
    env = counters(scrape, "envoy-stats")
    if env:
        g = lambda k: int(env.get(k, 0))
        print(f"  Envoy (summed across restarts): downstream requests {g('http.ingress.downstream_rq_total')} "
              f"(2xx {g('http.ingress.downstream_rq_2xx')}, 4xx {g('http.ingress.downstream_rq_4xx')}, 5xx {g('http.ingress.downstream_rq_5xx')}); "
              f"upstream requests {g('cluster.backend.upstream_rq_total')}, Envoy's own retries {g('cluster.backend.upstream_rq_retry')}, "
              f"upstream resets {g('cluster.backend.upstream_rq_rx_reset') + g('cluster.backend.upstream_rq_tx_reset')}, "
              f"connections destroyed with requests active {g('cluster.backend.upstream_cx_destroy_local_with_active_rq') + g('cluster.backend.upstream_cx_destroy_remote_with_active_rq')}")
    aq = counters(scrape, "aquifer-metrics")
    if aq:
        disp = {k.split("status_class=")[1].strip('"}'): int(v) for k, v in aq.items() if k.startswith("aquifer_dispatch_attempts_total") and "status_class=" in k}
        rej = {k.split("reason=")[1].strip('"}'): int(v) for k, v in aq.items() if k.startswith("aquifer_admission_rejected_total") and "reason=" in k}
        print(f"  Aquifer (summed across restarts): requests to Hatchet by status {disp}, per document {sum(disp.values()) / max(offered, 1):.2f}; admission refusals {rej}")
    if first_event is not None:
        for target in ("envoy-ready", "aquifer-ready"):
            down, up = downtime(scrape, target, first_event - 1)
            if down is not None:
                print(f"  {target.split('-')[0]}: not ready {down - first_event:+.0f} s after the first event, ready again "
                      f"{'%.0f s after it' % (up - first_event) if up else 'never in this run'}")
        rows = [json.loads(l) for l in open(os.path.join(run, "intervals.jsonl"))]
        series = [(ts(r["at"]), sum(t.get("finished", {}).get("completed", 0) for t in r["tenants"].values()), r.get("outstanding", 0)) for r in rows]
        last_event = ts(events[-1].split(" ")[0])
        gap = [t for t, n, _ in series if first_event <= t and n == 0]
        resumed = next((t - last_event for t, n, _ in series if t > last_event and n > 0), None)
        level = max((o for t, _, o in series if first_event - 60 <= t < first_event), default=0)
        cleared = next((t - last_event for t, _, o in series if t > last_event and o <= level), None)
        print(f"  completions: {len(gap) * 10} s of 10-s windows with none after the first event; resumed "
              f"{'%.0f s' % resumed if resumed is not None else 'never'} after the last event; backlog back to its pre-failure level "
              f"{'%.1f min' % (cleared / 60) if cleared is not None else 'never'} after the last event")
