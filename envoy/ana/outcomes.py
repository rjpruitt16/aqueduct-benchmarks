"""Sort every job in a survival run by when its work finished against its caller's deadline.

The deadline is the job's creation plus the run's patience. The work's finish
is its first execution at the backend (executions.txt); the caller's outcome
and when it ended come from the clients' job logs (JOB_LOG=1).

  answered        the caller got the result before its deadline
  not retrieved   the work finished before the deadline, the caller didn't get it
  finished late   the work finished after the deadline
  never ran       the backend never ran it
"""
import gzip, glob, json, os, sys


def openz(path):
    """Opens path, or path.gz when the file was compressed for the repo."""
    if not os.path.exists(path) and os.path.exists(path + ".gz"):
        return gzip.open(path + ".gz", "rt")
    return gzip.open(path, "rt") if path.endswith(".gz") else open(path)


def dur_ms(s):
    units = {"ms": 1, "s": 1000, "m": 60000, "h": 3600000}
    for u in ("ms", "s", "m", "h"):
        if s.endswith(u) and s[: -len(u)].replace(".", "").isdigit():
            return float(s[: -len(u)]) * units[u]
    raise ValueError(s)


def pct(v, n):
    return f"{100 * v / n:5.1f}%" if n else "    -"


def quantile(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))] if xs else 0


for run in sys.argv[1:]:
    info = dict(l.strip().split("=", 1) for l in open(os.path.join(run, "run.txt")) if "=" in l)
    patience = dur_ms(info["patience"])
    finished = {}
    for line in openz(os.path.join(run, "executions.txt")):
        parts = line.split()
        if len(parts) == 2:
            finished.setdefault(parts[0], int(parts[1]))
    jobs = []
    for f in sorted(glob.glob(run + "/clients/*.json*")):
        d = json.load(openz(f))
        # A job's ID is <client name>-<tenant>-<sequence>; tenants may hold "-".
        prefix = d["name"] + "-"
        jobs += [(j[0][len(prefix):].rsplit("-", 1)[0], *j) for j in d.get("jobs") or []]
    if not jobs:
        print(f"== {os.path.basename(run)}: no job log (run before JOB_LOG)")
        continue
    total, by_tenant, lag = {}, {}, []
    for tenant, jid, created, ended, outcome in jobs:
        deadline = created + patience
        done = finished.get(jid)
        if outcome == "ok" and ended <= deadline:
            bucket = "answered"
            if done is not None:
                lag.append(ended - done)
        elif done is None:
            bucket = "never ran"
        elif done <= deadline:
            bucket = "not retrieved"
        else:
            bucket = "finished late"
        for d in (total, by_tenant.setdefault(tenant, {})):
            d[bucket] = d.get(bucket, 0) + 1
    buckets = ["answered", "not retrieved", "finished late", "never ran"]
    hold = "hold" if info.get("hold") == "1" else "poll" if info.get("poll") == "1" else "backoff"
    print(f"== {os.path.basename(run)}  ({info.get('mode')}, D={info.get('scale_delay_min')} min, {hold} client)")
    n = len(jobs)
    print("  " + "  ".join(f"{b} {total.get(b, 0)} ({pct(total.get(b, 0), n).strip()})" for b in buckets))
    print(f"  finish to answer, answered jobs: p50 {quantile(lag, 0.5) / 1000:.1f}s  p99 {quantile(lag, 0.99) / 1000:.1f}s")
    for tenant in sorted(by_tenant):
        t = by_tenant[tenant]
        m = sum(t.values())
        print(f"    {tenant:8} " + "  ".join(f"{b} {pct(t.get(b, 0), m)}" for b in buckets))
