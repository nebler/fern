#!/usr/bin/env python3
"""Prints p50/p95 tables (Markdown) from results/*.jsonl.

  python3 -I lab/spike/runsc/summarize.py lab/spike/runsc/results
"""
import glob
import json
import math
import os
import sys


def pct(xs, p):
    xs = sorted(xs)
    return xs[max(0, math.ceil(p * len(xs)) - 1)]


def get(row, path):
    for k in path.split("."):
        row = row[int(k)] if isinstance(row, list) else row[k]
    return row


def table(name, rows, cols):
    print(f"\n### {name} (n={len(rows)})\n")
    print("| metric | p50 | p95 |")
    print("|---|---:|---:|")
    for label, path, scale in cols:
        xs = [get(r, path) * scale for r in rows]
        print(f"| {label} | {pct(xs, .5):,.1f} | {pct(xs, .95):,.1f} |")


MB = 1 / 1024
COLD = [
    ("netns + veth setup, ms", "net_ms", 1),
    ("create+start returned, wall ms", "start.wall_ms", 1),
    ("ready (/readyz 200), wall ms", "ready.wall_ms", 1),
    ("ready, CPU ms (all)", "ready.cpu_ms", 1),
    ("ready, CPU ms (runsc CLIs)", "ready.cli_ms", 1),
    ("CPU in the 3 s after ready, ms", "settle.cpu_ms", 1),
    ("first exec `true`, ms", "exec_ms", 1),
    ("exec `psql select 1`, ms", "exec2_ms", 1),
    ("PSS at ready, MB", "mem_ready.pss_kb", MB),
    ("PSS settled, MB", "mem_settled.pss_kb", MB),
    ("cgroup memory settled, MB", "mem_settled.cg_kb", MB),
    ("sleep (POST /stop, wait, delete), wall ms", "stop.wall_ms", 1),
    ("sleep, CPU ms", "stop.cpu_ms", 1),
]
CKPT = [
    ("checkpoint returned, wall ms", "ckpt.wall_ms", 1),
    ("checkpoint, CPU ms", "ckpt.cpu_ms", 1),
    ("sandbox exited + deleted, wall ms", "deleted.wall_ms", 1),
    ("sandbox exited + deleted, CPU ms", "deleted.cpu_ms", 1),
    ("image on disk, MB", "image_kb.1", MB),
    ("PSS before, MB", "mem_before.pss_kb", MB),
]
RESTORE = [
    ("restore returned, wall ms", "restored.wall_ms", 1),
    ("restore returned, CPU ms", "restored.cpu_ms", 1),
    ("ready (/readyz 200), wall ms", "ready.wall_ms", 1),
    ("ready, CPU ms (all)", "ready.cpu_ms", 1),
    ("ready, CPU ms (runsc CLIs)", "ready.cli_ms", 1),
    ("CPU in the 3 s after ready, ms", "settle.cpu_ms", 1),
    ("first exec (cat + psql), ms", "exec_ms", 1),
    ("app memory loaded at restore return (shmem), MB", "mem_restored.cg_shmem_kb", MB),
    ("PSS at ready, MB", "mem_ready.pss_kb", MB),
    ("PSS settled, MB", "mem_settled.pss_kb", MB),
    ("cgroup memory settled, MB", "mem_settled.cg_kb", MB),
]


SUB = [
    ("warm sandbox start (outside the claim), wall ms", "warm.wall_ms", 1),
    ("warm sandbox start, CPU ms", "warm.cpu_ms", 1),
    ("idle warm sandbox PSS, MB", "mem_warm.pss_kb", MB),
    ("idle warm sandbox cgroup memory, MB", "mem_warm.cg_kb", MB),
    ("claim: create+start subcontainer, wall ms", "started.wall_ms", 1),
    ("claim to ready (/readyz 200), wall ms", "ready.wall_ms", 1),
    ("claim to ready, CPU ms (all)", "ready.cpu_ms", 1),
    ("PSS after claim, MB", "mem_claimed.pss_kb", MB),
]


def valid(variant, kb):
    """Drops the big box's trial 0: its 400 MB process never started (a
    stale /tmp marker from a smoke test; bundles get a fresh /tmp now)."""
    return "big" not in variant or kb > 100 * 1024


def main():
    d = sys.argv[1]
    for f in sorted(glob.glob(f"{d}/*.jsonl")):
        name = os.path.basename(f)[:-6]
        rows = [json.loads(l) for l in open(f) if l.startswith("{")]
        if name.startswith("cold"):
            table(name, rows, COLD)
        elif name == "ckpt":
            # Rows from before "variant" was recorded come in run.sh's order.
            order = ["plain", "flate", "direct", "zero", "big"]
            for k, r in enumerate(rows):
                r.setdefault("variant", order[min(k // 15, 4)])
            rows = [r for r in rows if valid(r["variant"], r["image_kb"][1])]
            for v in order:
                table(f"checkpoint {v}", [r for r in rows if r["variant"] == v], CKPT)
        elif name.startswith("restore-"):
            rows = [r for r in rows if valid(name, r["mem_restored"]["cg_shmem_kb"])]
            table(name, rows, RESTORE)
            bad = [r["i"] for r in rows if not r["ok"]]
            print(f"\ncorrect after restore: {len(rows) - len(bad)}/{len(rows)}")
        elif name == "subcontainer":
            table("warm-pool claim (subcontainer)", rows, SUB)
            print(f"\nexec + workspace write ok: {sum(r['exec_ok'] for r in rows)}/{len(rows)}")
        else:
            print(f"\n{name}: {len(rows)} rows (not tabulated)")


if __name__ == "__main__":
    main()
