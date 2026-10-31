"""Reads a calibration run (a ramp of arrival rates) and prints, as shell
assignments:

CAPACITY   X, the workflow arrival rate that keeps the model fully busy. For
           each ramp phase, busy = mean running sequences / the model's
           capacity (LLM_MAX) while the phase's workflows were arriving.
           Below saturation busy grows in proportion to the rate, so
           X = rate / busy, from the busiest phase under 85% (the most
           signal, before queueing bends the line). If every phase is past
           85%, the lowest phase's estimate (an underestimate).
BUSY       each phase's rate and busy share, for the record
FIX_RATE   the share of finished workflows (completed or failed) with a
           verified fix: how well the model fixes the corpus
PER_ISSUE  completed/finished per issue

Usage: python3 -I calibrate.py <calibration run directory>
"""
import json
import os
import re
import sys
from datetime import datetime

d = sys.argv[1]
ledger = [json.loads(l) for l in open(os.path.join(d, "ledger.jsonl"))]
run = dict(l.rstrip("\n").split("=", 1) for l in open(os.path.join(d, "run.txt")) if "=" in l)
llm_max = int(run.get("llm_max", "8"))
rates = {}
for p in run["phases"].split(","):
    f = p.split(":")
    rates[f[0]] = float(f[2])

# Each phase's window: from its first submission to its last.
windows = {}
for e in ledger:
    t = datetime.fromisoformat(e["submitted"].replace("Z", "+00:00")).timestamp()
    lo, hi = windows.get(e["phase"], (t, t))
    windows[e["phase"]] = (min(lo, t), max(hi, t))

running = []  # (time, running sequences)
t = None
for line in open(os.path.join(d, "samples.txt")):
    if line.startswith("# t="):
        t = int(line[4:])
    m = re.match(r"^vllm:num_requests_running(?:\{[^}]*\})? ([0-9.eE+-]+)$", line.strip())
    if m and t is not None:
        running.append((t, float(m.group(1))))

busy = {}
for ph, (lo, hi) in windows.items():
    # Skip the phase's first 20 s: the running count lags the arrivals.
    xs = [r for (ts, r) in running if lo + 20 <= ts <= hi]
    if xs and ph in rates:
        busy[ph] = sum(xs) / len(xs) / llm_max

cands = [(b, rates[p] / b) for p, b in busy.items() if 0.05 < b < 0.85]
if cands:
    capacity = max(cands)[1]
else:
    sat = [(rates[p], rates[p] / b) for p, b in busy.items() if b >= 0.85]
    capacity = min(sat)[1] if sat else 0.0

finished = [e for e in ledger if e["outcome"] in ("completed", "failed", "unverified")]
fixed = sum(1 for e in finished if e["outcome"] == "completed")
issues = {}
for e in finished:
    c, n = issues.get(e["issue"], (0, 0))
    issues[e["issue"]] = (c + (e["outcome"] == "completed"), n + 1)
print(f"CAPACITY={capacity:.4f}")
print("BUSY='" + " ".join(f"{p}@{rates[p]}={b:.2f}" for p, b in sorted(busy.items(), key=lambda kv: rates[kv[0]])) + "'")
print(f"FIX_RATE={fixed / len(finished) if finished else 0:.3f}")
print("PER_ISSUE='" + " ".join(f"{k}={c}/{n}" for k, (c, n) in sorted(issues.items())) + "'")
