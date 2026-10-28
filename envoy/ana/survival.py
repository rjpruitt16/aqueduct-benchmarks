"""Summarize survival-regime runs: callers answered in time, work run, cost, and use of new capacity."""
import glob, json, os, re, sys

for run in sys.argv[1:]:
    info = dict(l.strip().split("=", 1) for l in open(os.path.join(run, "run.txt")) if "=" in l) if os.path.exists(os.path.join(run, "run.txt")) else {}
    tenants = {}
    for f in sorted(glob.glob(run + "/clients/*.json")):
        if os.path.basename(f).startswith("svc-"):
            continue
        d = json.load(open(f))
        tenants.update(d["tenants"])
    off = sum(t["offered"] for t in tenants.values())
    done = sum(t["completed"] for t in tenants.values())
    gave = sum(t["failed"].get("gave_up", 0) for t in tenants.values())
    other = sum(v for t in tenants.values() for k, v in t["failed"].items() if k != "gave_up")
    req = sum(t["requests"] for t in tenants.values())
    bs = json.load(open(run + "/backend-summary.json"))
    ran = bs["unique_jobs"]
    dup = sum(t["duplicates"] for t in bs["tenants"].values())
    # Capacity use after scaling, while the overload lasts.
    m = re.match(r"(\d+)m:(\d+)", info.get("capacity_schedule", "") or "")
    phases = info.get("phases", "")
    over_end = None
    if phases:
        t = 0
        for p in phases.split(","):
            n, dur, _ = p.split(":")
            secs = int(dur[:-1]) * (60 if dur.endswith("m") else 1)
            t += secs
            if n == "overload":
                over_end = t
    use = ""
    if m and over_end:
        at = int(m.group(1)) * 60
        rows = [json.loads(l) for l in open(run + "/backend.jsonl")]
        rows = [r for r in rows if at + 30 < r["t"] <= over_end]
        if rows:
            use = f"{sum(r['busy'] for r in rows) / len(rows):.1f}/{rows[-1]['workers']} busy after scaling"
    p50 = {k: round(sum(t['latency_p50_ms'].get('overload', 0) for t in tenants.values()) / 1000 / len(tenants), 1) for k in ["overload"]}
    print(f"== {os.path.basename(run)}  ({info.get('mode', '?')}, D={info.get('scale_delay_min', '?')} min, advertise={info.get('advertise', '?')}, no_key={info.get('no_key', '0')})")
    print(f"  offered {off}  answered in time {done} ({100 * done / max(off, 1):.1f}%)  gave up {gave}  other failures {other}")
    print(f"  ran at the backend {ran} ({100 * ran / max(off, 1):.1f}%)  repeats {dup}  requests per job {req / max(off, 1):.2f}  {use}")
    for name in sorted(tenants):
        t = tenants[name]
        print(f"    {name:8} answered {100 * t['completed'] / max(t['offered'], 1):5.1f}%  gave up {t['failed'].get('gave_up', 0):6}  p50 {t['latency_p50_ms'].get('overload', 0) / 1000:6.1f}s  p99 {t['latency_p99_ms'].get('overload', 0) / 1000:6.1f}s (overload)  req/job {t['requests'] / max(t['offered'], 1):5.2f}")
