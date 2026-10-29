"""Reads a calibration run's ledger and prints, as shell assignments:

CAPACITY   workflows completed per second at the best 2-minute window: what
           the stack sustains when the model is saturated
FIX_RATE   the share of finished workflows (completed or failed) with a
           verified fix: how well the model fixes the corpus
PER_ISSUE  completed/finished per issue

Usage: python3 -I calibrate.py <ledger.jsonl>
"""
import json
import sys
from datetime import datetime

ledger = [json.loads(l) for l in open(sys.argv[1])]
ends = []
for e in ledger:
    if e["outcome"] == "completed":
        t = datetime.fromisoformat(e["submitted"].replace("Z", "+00:00")).timestamp()
        ends.append(t + e["end_ms"] / 1000)
ends.sort()
best = 0.0
for i, t in enumerate(ends):
    n = sum(1 for u in ends[i:] if u < t + 120)
    best = max(best, n / 120)
finished = [e for e in ledger if e["outcome"] in ("completed", "failed", "unverified")]
fixed = sum(1 for e in finished if e["outcome"] == "completed")
issues = {}
for e in finished:
    c, n = issues.get(e["issue"], (0, 0))
    issues[e["issue"]] = (c + (e["outcome"] == "completed"), n + 1)
print(f"CAPACITY={best:.4f}")
print(f"FIX_RATE={fixed / len(finished) if finished else 0:.3f}")
print("PER_ISSUE='" + " ".join(f"{k}={c}/{n}" for k, (c, n) in sorted(issues.items())) + "'")
