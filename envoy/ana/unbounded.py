"""Summarize survival runs whose callers retry until their job is done (UNBOUNDED=1).

For each run: how much work finished, how long each caller waited for its
result (from creating the job to having the result), when the last job
offered during the overload got its result (the backlog clearing, from the
start of the overload and from when capacity arrived), requests per job, and
repeated work at the backend.
"""
import gzip, glob, json, os, sys


def openz(path):
    """Opens path, or path.gz when the file was compressed for the repo."""
    if not os.path.exists(path) and os.path.exists(path + ".gz"):
        return gzip.open(path + ".gz", "rt")
    return gzip.open(path, "rt") if path.endswith(".gz") else open(path)


def quantile(xs, q):
    return xs[min(len(xs) - 1, int(q * len(xs)))] if xs else 0


def minutes(phase):
    """Minutes in a phase length such as 3m or 90s."""
    return float(phase[:-1]) / 60 if phase.endswith("s") else float(phase[:-1])


for run in sys.argv[1:]:
    info = dict(l.strip().split("=", 1) for l in open(os.path.join(run, "run.txt")) if "=" in l)
    start = int(info["start_at"]) * 1000
    warm = minutes(info["phases"].split(",")[0].split(":")[1])
    overload_at = start + warm * 60000
    overload_end = overload_at + minutes(info["phases"].split(",")[1].split(":")[1]) * 60000
    capacity_at = start + minutes(info["capacity_schedule"].split(":")[0]) * 60000
    offered = requests = polite_offered = polite_requests = 0
    open_at_end = 0
    waits, by_tenant, outcomes, ends = [], {}, {}, []
    for f in sorted(glob.glob(run + "/clients/*.json*")):
        d = json.load(openz(f))
        open_at_end += d.get("open_at_end", 0)
        for t, v in d["tenants"].items():
            offered += v["offered"]
            requests += v["requests"]
            if t != "small-4":
                polite_offered += v["offered"]
                polite_requests += v["requests"]
        prefix = d["name"] + "-"
        for jid, created, ended, outcome in d.get("jobs") or []:
            tenant = jid[len(prefix):].rsplit("-", 1)[0]
            outcomes[outcome] = outcomes.get(outcome, 0) + 1
            if outcome == "ok":
                w = (ended - created) / 1000
                waits.append(w)
                by_tenant.setdefault(tenant, []).append(w)
                if created < overload_end:
                    ends.append(ended)
    bs = json.load(open(run + "/backend-summary.json"))
    repeats = sum(t["duplicates"] for t in bs["tenants"].values())
    done = outcomes.get("ok", 0)
    waits.sort()
    last = max(ends) if ends else start
    print(f"== {os.path.basename(run)}  ({info['mode']}, D={info.get('scale_delay_min')} min, hold={info.get('hold')}, patience={info.get('patience')})")
    print(f"  offered {offered}  completed {done} ({100 * done / offered:.1f}%)  outcomes {outcomes}  open at end {open_at_end}")
    print(f"  ran at the backend {bs['unique_jobs']}  repeats {repeats}  requests per job {requests / offered:.2f}  polite {polite_requests / polite_offered:.2f}")
    print(f"  time to result: p50 {quantile(waits, .5):.1f}s  p99 {quantile(waits, .99):.1f}s  max {waits[-1] if waits else 0:.1f}s")
    print(f"  overload's last result {(last - overload_at) / 60000:.1f} min after it began, {(last - capacity_at) / 60000:.1f} min after capacity arrived")
    for t in sorted(by_tenant, key=lambda t: -len(by_tenant[t])):
        w = sorted(by_tenant[t])
        print(f"    {t:8} p50 {quantile(w, .5):6.1f}s  p99 {quantile(w, .99):6.1f}s  max {w[-1]:6.1f}s")
