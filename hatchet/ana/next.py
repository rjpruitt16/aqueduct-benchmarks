"""Summarize Hatchet runs from the NEXT.md experiments (deploy/fly/next.sh).

For each run directory: work done, requests per document, latency for the
law firm and the small tenants during the overload, Postgres CPU and
documents completed per Postgres CPU-second, and, when Postgres was resized
or stopped (events.txt), throughput before, during and after.

Usage: python3 -I ana/next.py results/<run> [...]
"""
import json, os, sys
from datetime import datetime

SMALL = ["small-1", "small-2", "small-3", "small-4", "small-5"]


def ts(s):
    """Seconds since the epoch for an RFC 3339 time with up to nanoseconds."""
    s = s.rstrip("Z")
    if "." in s:
        head, frac = s.split(".")
        s = head + "." + frac[:6]
    return datetime.fromisoformat(s + "+00:00").timestamp()


def read(path, default=None):
    try:
        return open(path).read()
    except OSError:
        return default


for run in sys.argv[1:]:
    name = os.path.basename(run.rstrip("/"))
    info = dict(l.strip().split("=", 1) for l in (read(run + "/run.txt", "") or "").splitlines() if "=" in l and " " not in l.split("=", 1)[0])
    raw = read(run + "/summary.json")
    if raw is None:
        print(f"== {name}: no summary.json")
        continue
    s = json.loads(raw)
    tenants = s["tenants"]
    offered = sum(sum(t["offered_by_phase"].values()) for t in tenants.values())
    done = s["totals"].get("completed", 0)
    attempts = sum(t["avg_attempts"] * sum(t["offered_by_phase"].values()) for t in tenants.values()) / max(offered, 1)
    law = tenants["law-firm"]
    small_p99 = max(tenants[t]["p99_ms_by_phase"].get("overload", 0) for t in SMALL) / 1000
    acc_p99 = tenants["accounting"]["p99_ms_by_phase"].get("overload", 0) / 1000
    d = s.get('drain_seconds', 0)
    drain = "never" if d < 0 else f"{d / 60:.1f} min"
    print(f"== {name}  ({info.get('config', '?')}, pg {info.get('pg_size', 'performance-2x')})")
    print(f"  offered {offered}  completed {done} ({100 * done / max(offered, 1):.2f}%)  "
          f"rejected {s['totals'].get('rejected', 0)}  unfinished {s.get('outstanding_at_end', 0)}  "
          f"duplicates {s.get('duplicate_completions', 0)}  drain {drain}")
    print(f"  producer requests per document {attempts:.2f}")
    met = read(run + "/aquifer_metrics.txt")
    if met:
        to_hatchet = sum(float(l.rsplit(" ", 1)[1]) for l in met.splitlines() if l.startswith("aquifer_dispatch_attempts_total"))
        print(f"  Aquifer's requests to Hatchet per document {to_hatchet / max(offered, 1):.2f}")
    # Workflow completion (offer to the last step finishing) for every tenant,
    # and, from drivers that record them, time to first acceptance, retries
    # and backoff, kept apart from completion.
    for t in ["law-firm", "accounting"] + SMALL:
        v = tenants.get(t)
        if not v:
            continue
        line = (f"    {t:10} completion p50/p99 {v['p50_ms_by_phase'].get('overload', 0) / 1000:8.1f}/{v['p99_ms_by_phase'].get('overload', 0) / 1000:8.1f} s")
        if "accept_p50_ms_by_phase" in v:
            line += (f" | first acceptance p50/p99 {v['accept_p50_ms_by_phase'].get('overload', 0) / 1000:7.1f}/{v['accept_p99_ms_by_phase'].get('overload', 0) / 1000:7.1f} s"
                     f" | run ID p50/p99 {v.get('to_run_id_p50_ms', 0) / 1000:7.1f}/{v.get('to_run_id_p99_ms', 0) / 1000:7.1f} s"
                     f" | retries {v['retries']} on {v['retried_documents']} docs, backoff {v['backoff_seconds']:.0f} s total,"
                     f" p99 {v['backoff_p99_ms_by_phase'].get('overload', 0) / 1000:.1f} s")
        else:
            line += f" | (no acceptance or backoff fields in this driver) attempts/doc {v['avg_attempts']:.2f}"
        print(line)
    print(f"  law firm p50/p99 in overload {law['p50_ms_by_phase'].get('overload', 0) / 60000:.1f}/{law['p99_ms_by_phase'].get('overload', 0) / 60000:.1f} min  "
          f"accounting p99 {acc_p99:.1f} s  small tenants p99 {small_p99:.1f} s")

    cpu = json.loads(read(run + "/cpu.json", "[]") or "[]")
    scale = 1.0
    if isinstance(cpu, list):
        # The first runs saved every non-idle mode in hundredths of a second.
        scale = 0.01
    else:
        cpu = cpu.get("machines", [])
    pg = [m for m in cpu if m.get("machine") == "pg"]
    if pg:
        vals = [float(v) * scale for _, v in pg[0]["values"]]
        cpu_s = sum(vals) * 30
        print(f"  Postgres CPU: mean {sum(vals) / len(vals):.2f} cores, peak {max(vals):.2f}, "
              f"{cpu_s / 3600:.2f} core-hours; {done / max(cpu_s, 1):.2f} documents per Postgres CPU-second")
        others = {m["machine"]: sum(float(v) * scale for _, v in m["values"]) * 30 for m in cpu if m.get("machine") != "pg"}
        if others:
            print("  other machines' CPU-seconds: " + ", ".join(f"{k} {v:.0f}" for k, v in sorted(others.items())))
    else:
        print("  no CPU data")

    events = (read(run + "/events.txt", "") or "").strip().splitlines()
    if events:
        print("  events: " + "; ".join(e.split(" ", 2)[1] + " " + e.split(" ", 2)[2] for e in events))
        rows = [json.loads(l) for l in open(run + "/intervals.jsonl")]
        series = []
        for r in rows:
            n = sum(t.get("finished", {}).get("completed", 0) for t in r["tenants"].values())
            series.append((ts(r["at"]), n, r.get("retries", 0)))
        first = ts(events[0].split(" ")[0])
        last = ts(events[-1].split(" ")[0])
        before = [n for t, n, _ in series if first - 300 <= t < first]
        rate = sum(before) / max(len(before), 1)
        during = [n for t, n, _ in series if first <= t <= last]
        retries = sum(r for t, _, r in series if first <= t <= last + 120)
        # Recovered: the first 1-minute window after the last event that
        # completes at least 90% of the pre-event rate.
        after = [(t, n) for t, n, _ in series if t > last]
        back = None
        for i in range(len(after) - 5):
            if sum(n for _, n in after[i:i + 6]) / 6 >= 0.9 * rate:
                back = after[i][0] - last
                break
        # Outage measures: when completions resumed after Postgres came back,
        # when the backlog was back to its level before the event, Postgres
        # CPU in the 10 minutes after, and the small tenants' worst p99 in
        # any 10 s window from the event to 10 minutes after.
        resumed = next((t - last for t, n, _ in series if t > last and n > 0), None)
        outstanding = [(ts(r["at"]), r.get("outstanding", 0)) for r in rows]
        before_out = [o for t, o in outstanding if first - 60 <= t < first]
        level = max(before_out) if before_out else 0
        cleared = next((t - last for t, o in outstanding if t > last and o <= level), None)
        pg_after = []
        if pg:
            pg_after = [float(v) * scale for t, v in pg[0]["values"] if last <= float(t) <= last + 600]
        small_p99 = max((r["tenants"].get(t, {}).get("p99_ms", 0) for r in rows if first <= ts(r["at"]) <= last + 600 for t in SMALL), default=0) / 1000
        lost = {k: v for k, v in s["totals"].items() if k != "completed"}
        outcomes = {}
        for t in tenants.values():
            for k, n in t["outcomes"].items():
                if k != "completed":
                    outcomes[k] = outcomes.get(k, 0) + n
        print(f"  not completed: {outcomes or 'none'}; completions resumed {'%.0f s' % resumed if resumed is not None else 'never'} after the last event; "
              f"backlog back to its pre-event level {'%.1f min' % (cleared / 60) if cleared is not None else 'never'} after; "
              f"Postgres CPU in the 10 min after {sum(pg_after) / max(len(pg_after), 1):.2f} cores; small tenants' worst 10 s p99 {small_p99:.1f} s")
        print(f"  completions per 10 s: {rate:.1f} in the 5 min before, {sum(during) / max(len(during), 1):.1f} during; "
              f"back to 90% {'%.0f s after' % back if back is not None else 'never'}; "
              f"producer retries from the first event to 2 min after the last {retries}")
