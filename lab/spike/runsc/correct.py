#!/usr/bin/env python3
"""Correctness of runsc checkpoint/restore for Fern-like boxes (spike #6).
Runs on the node as root; prints one JSON line per case.

  correct.py host-edit   files changed on the host between checkpoint and
                         restore, one of them held open by a box process
  correct.py revert      the workspace swapped for an older snapshot (a
                         Fern restore to an earlier turn), then restore
  correct.py new-ip      restore into a netns with a different address
"""
import json
import os
import subprocess
import sys
import time

import box

S = box.S
N = 13


def start(bid, n):
    box.destroy(bid)  # a leftover of an interrupted run
    ws = f"{S}/ws/{bid}"
    box.snapshot(f"{S}/golden", ws)
    box.net_up(n)
    box.create_start(bid, box.bundle(bid, n, ws))
    box.wait_ready(n, 60)
    return ws


def run(n, script):
    r = box.bexec(n, "sh", "-c", script)
    return {"rc": r.returncode, "out": r.stdout.strip(), "err": r.stderr.strip().splitlines()[1:]}


def checkpoint(bid, img):
    box.sh("rm", "-rf", img)
    os.makedirs(img)
    box.runsc("checkpoint", f"--image-path={img}", bid)
    box.runsc("wait", bid, check=False)
    box.destroy(bid)


def restore(bid, img, n=N):
    # The sentry took the netns's addresses when it booted; a restored one
    # reads them again, so the netns is set up afresh (as a CNI ADD would).
    box.net_up(n)
    with box.logf(bid) as log:
        r = subprocess.run(box.RUNSC + ["restore", "--detach", f"--bundle={S}/bundle/{bid}", f"--image-path={img}", bid],
                           stdin=subprocess.DEVNULL, stdout=log, stderr=log)
    return r.returncode


def log_tail(bid, k=6):
    with open(f"{S}/logs/{bid}.log") as f:
        return [l.rstrip()[:240] for l in f.readlines()[-k:]]


def ready(n, timeout):
    try:
        box.wait_ready(n, timeout)
        return True
    except Exception as e:  # noqa: BLE001 — reported, not handled
        return repr(e)[:200]


def host_edit():
    bid, img = "c-edit", f"{S}/ckpt/c-edit"
    ws = start(bid, N)
    run(N, "echo before > edited.txt; echo before > deleted.txt; echo before > held.txt; echo before > replaced.txt;"
           "psql -Atc 'create table k(v int); insert into k values (42)';"
           # A process holding held.txt and replaced.txt open across the checkpoint.
           "setsid nohup sh -c 'exec 3<held.txt 4<replaced.txt; while :; do sleep 0.2; done' >/dev/null 2>&1 &"
           "sleep 0.3")
    checkpoint(bid, img)
    # On the host, in the subvolume, between checkpoint and restore.
    with open(f"{ws}/edited.txt", "w") as f:
        f.write("changed on the host, longer\n")
    with open(f"{ws}/held.txt", "a") as f:
        f.write("appended on the host\n")
    with open(f"{ws}/replaced.txt.new", "w") as f:
        f.write("replacement\n")
    os.rename(f"{ws}/replaced.txt.new", f"{ws}/replaced.txt")
    os.remove(f"{ws}/deleted.txt")
    with open(f"{ws}/host-new.txt", "w") as f:
        f.write("new on the host\n")
    for p in ("edited.txt", "held.txt", "replaced.txt", "host-new.txt"):
        os.chown(f"{ws}/{p}", 1000, 1000)
    rc = restore(bid, img)
    row = {"case": "host-edit", "restore_rc": rc}
    if rc == 0:
        row["ready"] = ready(N, 15)
        row["box_sees"] = run(N, "for f in edited.txt deleted.txt held.txt replaced.txt host-new.txt; do"
                                 " printf '%s: ' $f; cat $f 2>&1 | tr '\\n' '|'; echo; done;"
                                 "psql -Atc 'select v from k'")
        # What the process that held the files open reads now, through its fds.
        row["held_fds"] = run(N, "p=$(grep -l 'exec 3<held' /proc/[0-9]*/cmdline | awk -F/ 'NR==1{print $3}');"
                                 "echo pid=$p; for fd in 3 4; do printf 'fd%s: ' $fd; cat /proc/$p/fd/$fd | tr '\\n' '|'; echo; done")
        row["box_write"] = run(N, "echo from-box > after.txt")
        row["box_write_reaches_host"] = os.path.exists(f"{ws}/after.txt")
    row["log"] = log_tail(bid)
    print(json.dumps(row), flush=True)
    box.destroy(bid)
    box.net_down(N)
    box.sh("btrfs", "subvolume", "delete", ws)


def revert():
    """Checkpoint at turn B, then restore onto the workspace of turn A."""
    bid, img = "c-revert", f"{S}/ckpt/c-revert"
    ws = start(bid, N)
    turn_a = ws + ".turnA"
    run(N, "psql -Atc 'create table k(v int); insert into k values (1)'; psql -Atc checkpoint; echo A > turn.txt")
    box.snapshot(ws, turn_a)
    run(N, "psql -Atc 'insert into k select generate_series(2, 20000)'; echo B > turn.txt")
    checkpoint(bid, img)
    # Fern's restore: swap the live subvolume for the snapshot of turn A.
    box.sh("btrfs", "subvolume", "delete", ws)
    box.snapshot(turn_a, ws)
    rc = restore(bid, img)
    row = {"case": "revert", "restore_rc": rc}
    if rc == 0:
        row["ready"] = ready(N, 15)
        row["box_sees"] = run(N, "cat turn.txt; psql -Atc 'select count(*), max(v) from k'; "
                                 "psql -Atc 'insert into k values (-1)'; psql -Atc checkpoint; psql -Atc 'select count(*) from k'")
        time.sleep(1)
    row["log"] = log_tail(bid, 10)
    print(json.dumps(row), flush=True)
    # A cold start on the same disk afterwards: does Postgres come up clean?
    box.destroy(bid)
    box.net_up(N)
    box.create_start(bid, box.bundle(bid, N, ws))
    row2 = {"case": "revert-then-cold-start", "ready": ready(N, 30)}
    if row2["ready"] is True:
        row2["box_sees"] = run(N, "cat turn.txt; psql -Atc 'select count(*), max(v) from k'")
    row2["log"] = log_tail(bid, 8)
    print(json.dumps(row2), flush=True)
    box.destroy(bid)
    box.net_down(N)
    for v in (ws, turn_a):
        box.sh("btrfs", "subvolume", "delete", v)


def new_ip():
    bid, img = "c-ip", f"{S}/ckpt/c-ip"
    ws = start(bid, N)
    checkpoint(bid, img)
    box.net_down(N)
    m = N + 1
    box.bundle(bid, m, ws, fresh_tmp=False)  # the same box, in a netns with 10.250.<m>.2
    rc = restore(bid, img, m)
    row = {"case": "new-ip", "restore_rc": rc}
    if rc == 0:
        row["ready_at_new_ip"] = ready(m, 5)
        r = box.sh("ip", "netns", "exec", f"spike-{m}", "ip", "addr", "show", "eth0", check=False)
        row["netns_eth0"] = [l.strip() for l in r.stdout.splitlines() if "inet" in l]
        row["box_ifaces"] = run(m, "cat /proc/net/fib_trie 2>/dev/null | awk 'NR<=12'; ip addr 2>/dev/null || cat /proc/net/if_inet6")
    row["log"] = log_tail(bid)
    print(json.dumps(row), flush=True)
    box.destroy(bid)
    box.net_down(m)
    box.sh("btrfs", "subvolume", "delete", ws)


def main():
    box.cg_setup()
    {"host-edit": host_edit, "revert": revert, "new-ip": new_ip}[sys.argv[1]]()


if __name__ == "__main__":
    main()
