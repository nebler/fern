#!/usr/bin/env python3
"""Measures cold start, checkpoint and restore of Fern-like boxes under plain
runsc (spike #6). Runs on the node as root; writes one JSON line per trial
to /var/lib/fern/spike/results/<phase>.jsonl.

  bench.py cold N          cold start to /readyz, then sleep (POST /stop)
  bench.py cold-cold-cache N   the same after dropping the page cache
  bench.py ckpt VARIANT N  cold start, write, checkpoint (keeps the images)
  bench.py restore VARIANT N
"""
import json
import os
import subprocess
import sys
import time

import box

S = box.S
OUT = f"{S}/results"
SETTLE = 3.0  # seconds after ready before the second memory/CPU reading

# Checkpoint variants: name -> runsc checkpoint flags.
CKPT = {
    "plain": [],
    "flate": ["--compression=flate-best-speed"],
    "direct": ["--direct"],
    "zero": ["--exclude-committed-zero-pages"],
    # A box with a 400 MB process (a dev server, an agent): random bytes, so
    # neither zero-page exclusion nor compression flatters it.
    "big": [],
}
BIG = ("sh", "-c", "setsid nohup node -e '"
       "const b = require(\"crypto\").randomFillSync(Buffer.allocUnsafe(400 << 20));"
       "require(\"fs\").writeFileSync(\"/tmp/big.ready\", \"\");"
       "setInterval(() => b[0]++, 60000)' >/dev/null 2>&1 & "
       "until [ -e /tmp/big.ready ]; do sleep 0.1; done")
# Restore variants: name -> (checkpoint variant, restore flags, cold cache).
RESTORE = {
    "plain": ("plain", [], False),
    "background": ("plain", ["--background"], False),
    "flate": ("flate", [], False),
    "direct": ("direct", ["--direct"], False),
    "zero": ("zero", [], False),
    "plain-cold": ("plain", [], True),
    "background-cold": ("plain", ["--background"], True),
    "big": ("big", [], False),
    "big-background": ("big", ["--background"], False),
    "big-cold": ("big", [], True),
    "big-background-cold": ("big", ["--background"], True),
}


def record(phase, row):
    os.makedirs(OUT, exist_ok=True)
    with open(f"{OUT}/{phase}.jsonl", "a") as f:
        f.write(json.dumps(row) + "\n")
    print(phase, json.dumps(row), flush=True)


class Total:
    """Wall and CPU of everything under the spike cgroup, minus this
    harness's own CPU (it sits in the same tree)."""

    def __enter__(self):
        self.t0 = time.perf_counter()
        self.c0, self.s0, self.l0 = box.total_usec(), box.self_usec(), box.cpu_usec("cli")
        return self

    def lap(self):
        own = box.self_usec() - self.s0
        return {"wall_ms": round((time.perf_counter() - self.t0) * 1000, 1),
                "cpu_ms": round(((box.total_usec() - self.c0) - own) / 1000, 1),
                # the runsc CLIs (and the sandbox until runsc moves it)
                "cli_ms": round(((box.cpu_usec("cli") - self.l0) - own) / 1000, 1)}

    def __exit__(self, *exc):
        return False


def cold_box(bid, n, ws, cold_cache=False):
    """Starts a box on a fresh snapshot of golden; returns its timings."""
    box.snapshot(f"{S}/golden", ws)
    t = time.perf_counter()
    box.net_up(n)
    net_ms = (time.perf_counter() - t) * 1000
    b = box.bundle(bid, n, ws)
    if cold_cache:
        drop_caches()
    with Total() as tot:
        box.create_start(bid, b)
        started = tot.lap()
        box.wait_ready(n)
        ready = tot.lap()
    return {"net_ms": round(net_ms, 1), "start": started, "ready": ready}


def exec_ms(n, *argv):
    t = time.perf_counter()
    r = box.bexec(n, *argv)
    return round((time.perf_counter() - t) * 1000, 1), r


def cleanup(bid, n, *subvols):
    box.destroy(bid)
    box.net_down(n)
    for v in subvols:
        if os.path.exists(v):
            box.sh("btrfs", "subvolume", "delete", v)


def cold(count, cold_cache=False):
    for i in range(count):
        bid, n, ws = f"cold{i}", 10, f"{S}/ws/cold{i}"
        row = {"i": i, "cold_cache": cold_cache, **cold_box(bid, n, ws, cold_cache)}
        row["mem_ready"] = box.mem(bid)
        with Total() as tot:
            time.sleep(SETTLE)
            row["settle"] = tot.lap()
        row["mem_settled"] = box.mem(bid)
        row["exec_ms"], r = exec_ms(n, "true")
        row["exec2_ms"], r = exec_ms(n, "psql", "-Atc", "select 1")
        assert r.returncode == 0 and r.stdout.strip() == "1", r
        with Total() as tot:
            box.stop(bid, n)
            row["stop"] = tot.lap()
        record("cold-cold-cache" if cold_cache else "cold", row)
        cleanup(bid, n, ws)


WRITE = ("sh", "-c", "echo before > before.txt; "
         "psql -Atc 'create table k(v int); insert into k values (42)'")


def ckpt(variant, count):
    flags = CKPT[variant]
    for i in range(count):
        bid, n, ws = f"ck-{variant}-{i}", 11, f"{S}/ws/ck-{variant}-{i}"
        row = {"i": i, "variant": variant, "flags": flags, **cold_box(bid, n, ws)}
        _, r = exec_ms(n, *WRITE)
        assert r.returncode == 0, r
        if variant == "big":
            _, r = exec_ms(n, *BIG)
            assert r.returncode == 0, r
        time.sleep(2)
        row["mem_before"] = box.mem(bid)
        img = f"{S}/ckpt/{variant}-{i}"
        if os.path.exists(img):
            box.sh("rm", "-rf", img)
        os.makedirs(img)
        with Total() as tot:
            box.runsc("checkpoint", f"--image-path={img}", *flags, bid)
            row["ckpt"] = tot.lap()
            box.runsc("wait", bid, check=False)
            row["exited"] = tot.lap()
            box.destroy(bid)
            row["deleted"] = tot.lap()
        # What Fern would snapshot at sleep: the workspace the image goes with.
        box.snapshot(ws, ws + ".at")
        row["image"] = {f: os.path.getsize(f"{img}/{f}") for f in os.listdir(img)}
        row["image_kb"] = box.du_kb(img)
        record("ckpt", row)
        cleanup(bid, n, ws)


def drop_caches():
    subprocess.run(["sync"], check=True)
    with open("/proc/sys/vm/drop_caches", "w") as f:
        f.write("3")


def restore_box(bid, n, ws, img, flags, cold_cache=False):
    """Restores img onto ws (re-created from its .at snapshot, as Fern's
    restore swaps subvolumes) and returns timings."""
    box.snapshot(ws + ".at", ws)
    box.net_up(n)
    if cold_cache:
        drop_caches()
    b = f"{S}/bundle/{bid}"
    row = {}
    with Total() as tot:
        with box.logf(bid) as log:
            subprocess.run(box.RUNSC + ["restore", "--detach", f"--bundle={b}", f"--image-path={img}", *flags, bid],
                           stdin=subprocess.DEVNULL, stdout=log, stderr=log, check=True)
        row["restored"] = tot.lap()
        row["mem_restored"] = box.mem(bid)
        box.wait_ready(n)
        row["ready"] = tot.lap()
    return row


def restore(variant, count):
    cv, flags, cold_cache = RESTORE[variant]
    for i in range(count):
        src = f"ck-{cv}-{i}"
        bid, n, ws, img = src, 11, f"{S}/ws/{src}", f"{S}/ckpt/{cv}-{i}"
        row = {"i": i, "flags": flags, "cold_cache": cold_cache, **restore_box(bid, n, ws, img, flags, cold_cache)}
        row["mem_ready"] = box.mem(bid)
        row["exec_ms"], r = exec_ms(n, "sh", "-c", "cat before.txt; psql -Atc 'select v from k'")
        row["ok"] = r.returncode == 0 and r.stdout.split() == ["before", "42"]
        if cv == "big":
            _, r = exec_ms(n, "sh", "-c", "grep -l [r]andomFillSync /proc/[0-9]*/cmdline 2>/dev/null | wc -l")
            row["ok"] = row["ok"] and r.stdout.strip() != "0"
        with Total() as tot:
            time.sleep(SETTLE)
            row["settle"] = tot.lap()
        row["mem_settled"] = box.mem(bid)
        row["exec2_ms"], _ = exec_ms(n, "true")
        record(f"restore-{variant}", row)
        cleanup(bid, n, ws)


def main():
    box.cg_setup()
    phase = sys.argv[1]
    if phase == "cold":
        cold(int(sys.argv[2]))
    elif phase == "cold-cold-cache":
        cold(int(sys.argv[2]), cold_cache=True)
    elif phase == "ckpt":
        ckpt(sys.argv[2], int(sys.argv[3]))
    elif phase == "restore":
        restore(sys.argv[2], int(sys.argv[3]))
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
