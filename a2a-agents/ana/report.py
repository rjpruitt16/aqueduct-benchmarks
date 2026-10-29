"""The pilot's report: one row per run, then each scenario's configurations
side by side (both repetitions, and their mean).

Usage: python3 -I ana/report.py <results-dir>   (writes REPORT.md there too)

Latency is end to end: from the driver submitting a workflow to its verified
result (sandbox and CI records checked). Only completed workflows have one.
Unfinished workflows (still running when the drain ended) are counted, never
dropped.
"""
import json
import os
import re
import sys
from statistics import mean

root = sys.argv[1]
RUN = re.compile(r"^(normal|burst|flood)-([ABC])-(\d+)$")


def pct(xs, q):
    if not xs:
        return None
    s = sorted(xs)
    return s[min(len(s) - 1, int(q * len(s)))]


def metric(text, name):
    m = re.search(r"^" + re.escape(name) + r"(?:\{[^}]*\})? ([0-9.eE+-]+)$", text, re.M)
    return float(m.group(1)) if m else None


def samples(path):
    """Mean of each sampled gauge over the run, and CPU busy share."""
    if not os.path.exists(path):
        return {}
    vals, cpu = {}, []
    for block in open(path).read().split("# t=")[1:]:
        for line in block.splitlines()[1:]:
            if line.startswith("cpu "):
                f = [int(x) for x in line.split()[1:]]
                cpu.append((sum(f), f[3] + f[4]))
                continue
            m = re.match(r"^([a-z_:]+)(?:\{[^}]*\})? ([0-9.eE+-]+)$", line)
            if m:
                vals.setdefault(m.group(1), []).append(float(m.group(2)))
    out = {k: mean(v) for k, v in vals.items() if not k.endswith("_total")}
    if len(cpu) > 1:
        total, idle = cpu[-1][0] - cpu[0][0], cpu[-1][1] - cpu[0][1]
        out["cpu_busy"] = 1 - idle / total if total else None
    return out


def run_row(name, cfg, scen, rep):
    d = os.path.join(root, name)
    ledger = [json.loads(l) for l in open(os.path.join(d, "ledger.jsonl"))]
    end_metrics = open(os.path.join(d, "metrics_end.txt")).read() if os.path.exists(os.path.join(d, "metrics_end.txt")) else ""
    llm_max = 8
    for line in open(os.path.join(d, "run.txt")):
        if line.startswith("llm_max="):
            llm_max = int(line.split("=", 1)[1])
    lat = {}
    out = {}
    reqs = {}
    backoff = 0
    for e in ledger:
        out[e["outcome"]] = out.get(e["outcome"], 0) + 1
        if e["outcome"] == "completed":
            lat.setdefault("all", []).append(e["end_ms"] / 1000)
            lat.setdefault(e["tenant"], []).append(e["end_ms"] / 1000)
            if e["phase"] == "burst":
                lat.setdefault("burst-phase", []).append(e["end_ms"] / 1000)
        for c in e.get("calls") or []:
            reqs[c["resource"]] = reqs.get(c["resource"], 0) + c["attempts"]
            backoff += c["backoff_ms"]
    done = out.get("completed", 0)
    s = samples(os.path.join(d, "samples.txt"))
    small = lat.get("small-1", []) + lat.get("small-2", [])
    return {
        "run": name, "config": cfg, "scenario": scen, "rep": rep,
        "submitted": len(ledger), "completed": done,
        "failed": out.get("failed", 0) + out.get("unverified", 0) + out.get("error", 0),
        "unfinished": out.get("unfinished", 0),
        "p50": pct(lat.get("all", []), 0.5), "p95": pct(lat.get("all", []), 0.95), "p99": pct(lat.get("all", []), 0.99),
        "big_p50": pct(lat.get("big", []), 0.5), "small_p50": pct(small, 0.5), "small_p99": pct(small, 0.99),
        "burst_p99": pct(lat.get("burst-phase", []), 0.99),
        "req_per_wf": sum(reqs.values()) / done if done else None,
        "llm_req_per_wf": reqs.get("llm", 0) / done if done else None,
        "ci_req_per_wf": reqs.get("ci", 0) / done if done else None,
        "backoff_s": backoff / 1000,
        "repeated_exec": metric(end_metrics, "sandbox_repeated_executions_total"),
        "ci_dup_starts": metric(end_metrics, "ci_duplicate_starts_total"),
        "ci_refused": metric(end_metrics, "ci_refused_total"),
        "sbx_refused": metric(end_metrics, "sandbox_refused_total"),
        "gpu_busy": (s.get("vllm:num_requests_running") or 0) / llm_max if s else None,
        "gpu_waiting": s.get("vllm:num_requests_waiting"),
        "cpu_busy": s.get("cpu_busy"),
    }


def f(v, nd=1):
    if v is None:
        return "–"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


rows = []
for name in sorted(os.listdir(root)):
    m = RUN.match(name)
    if m and os.path.exists(os.path.join(root, name, "ledger.jsonl")):
        rows.append(run_row(name, m.group(2), m.group(1), int(m.group(3))))

cols = [("run", 0), ("submitted", 0), ("completed", 0), ("failed", 0), ("unfinished", 0), ("p50", 0), ("p99", 0),
        ("small_p99", 0), ("burst_p99", 0), ("req_per_wf", 1), ("ci_req_per_wf", 1), ("backoff_s", 0),
        ("repeated_exec", 0), ("ci_refused", 0), ("gpu_busy", 2), ("gpu_waiting", 1), ("cpu_busy", 2)]
lines = ["# A2A pilot report", ""]
cal = os.path.join(root, "calibration.txt")
if os.path.exists(cal):
    lines += ["Calibration: `" + open(cal).read().strip() + "`", ""]
lines += ["Latency in seconds, end to end, completed workflows only. `small_p99`: the two small tenants together.",
          "`burst_p99`: workflows submitted during the burst. `gpu_busy`: mean running sequences / capacity.", ""]
lines += ["## Every run", "", "| " + " | ".join(c for c, _ in cols) + " |", "|" + "---|" * len(cols)]
for r in rows:
    lines.append("| " + " | ".join(f(r[c], nd) for c, nd in cols) + " |")

keys = [("completed", 0), ("unfinished", 0), ("p50", 0), ("p99", 0), ("big_p50", 0), ("small_p50", 0), ("small_p99", 0),
        ("burst_p99", 0), ("req_per_wf", 1), ("llm_req_per_wf", 1), ("ci_req_per_wf", 1), ("backoff_s", 0),
        ("repeated_exec", 0), ("ci_dup_starts", 0), ("ci_refused", 0), ("sbx_refused", 0), ("gpu_busy", 2), ("cpu_busy", 2)]
for scen in ("normal", "burst", "flood"):
    rs = [r for r in rows if r["scenario"] == scen]
    if not rs:
        continue
    lines += ["", f"## {scen}", "", "Each cell: the runs' values, then their mean.", "",
              "| | " + " | ".join(c for c in "ABC") + " |", "|---|---|---|---|"]
    for k, nd in keys:
        cells = []
        for cfg in "ABC":
            vs = [r[k] for r in sorted(rs, key=lambda r: r["rep"]) if r["config"] == cfg]
            num = [v for v in vs if v is not None]
            cell = " / ".join(f(v, nd) for v in vs)
            if len(num) > 1:
                cell += f" (mean {f(mean(num), nd)})"
            cells.append(cell or "–")
        lines.append(f"| {k} | " + " | ".join(cells) + " |")

text = "\n".join(lines) + "\n"
open(os.path.join(root, "REPORT.md"), "w").write(text)
print(text)
