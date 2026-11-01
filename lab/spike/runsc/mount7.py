#!/usr/bin/env python3
"""Spike #7: can a thread's workspace (a btrfs subvolume) reach an already
running gVisor sandbox (a warm pool)? Runs on the node as root.

  mount7.py propagate OPT   a slot directory bound into the box with
                            propagation OPT (rshared, rslave, rprivate);
                            the subvolume is bind-mounted under it on the
                            host after the box is up
  mount7.py subcontainer N  a warm sandbox (pause-like root container),
                            claimed by adding fern-init as a subcontainer
                            with the workspace mount: N timed claims
"""
import json
import os
import subprocess
import sys
import time

import box

S = box.S
N = 15


def run(n, script):
    r = box.bexec(n, "sh", "-c", script)
    return {"rc": r.returncode, "out": r.stdout.strip().splitlines(), "err": r.stderr.strip().splitlines()[1:]}


def propagate(opt):
    bid = f"m7-{opt}"
    slot, claim, empty = f"{S}/slot/{bid}", f"{S}/ws/{bid}-claim", f"{S}/ws/{bid}-empty"
    os.makedirs(f"{slot}/ws", exist_ok=True)
    os.makedirs(f"{slot}/late", exist_ok=True)
    # The slot is a mount of its own, shared, so mounts under it propagate.
    box.sh("mount", "--bind", slot, slot)
    box.sh("mount", "--make-shared", slot)
    os.makedirs(empty, exist_ok=True)
    os.chown(empty, 1000, 1000)
    box.snapshot(f"{S}/golden", claim)
    box.net_up(N)
    b = box.bundle(bid, N, empty, extra_mounts=[
        {"destination": "/slot", "type": "bind", "source": slot, "options": ["rbind", "rw", opt]}])
    box.create_start(bid, b)
    box.wait_ready(N)
    row = {"case": "propagate", "opt": opt, "runsc_flags": box.RUNSC[2:]}
    row["guest_mounts"] = run(N, "grep -e slot -e ' / ' /proc/mounts")
    # Prime the guest's dentry cache with the empty directory.
    row["before"] = run(N, "ls -A /slot/ws | wc -l; stat -c '%d %i' /slot/ws")
    t = time.perf_counter()
    box.sh("mount", "--bind", claim, f"{slot}/ws")
    row["host_mount_ms"] = round((time.perf_counter() - t) * 1000, 2)
    # And a directory the guest never looked at before the mount.
    box.sh("mount", "--bind", claim, f"{slot}/late")
    row["gofer_sees"] = []
    for pid in box.procs(bid):
        try:
            if open(f"/proc/{pid}/comm").read().strip().startswith("runsc-gofer"):
                row["gofer_sees"] = [l.split()[4] for l in open(f"/proc/{pid}/mountinfo") if "slot" in l or "ws" in l][:6]
        except OSError:
            pass
    row["after_primed"] = run(N, "ls -A /slot/ws | wc -l; stat -c '%d %i' /slot/ws; head -c 40 /slot/ws/services.yaml")
    row["after_late"] = run(N, "ls -A /slot/late | wc -l; head -c 40 /slot/late/services.yaml")
    if row["after_primed"]["out"][:1] not in (["0"], []):
        run(N, "echo from-box > /slot/ws/from-box.txt")
        row["box_write_on_host"] = {
            "in_subvolume": os.path.exists(f"{claim}/from-box.txt"),
        }
        with open(f"{claim}/from-host.txt", "w") as f:
            f.write("from-host\n")
        row["host_write_in_box"] = run(N, "cat /slot/ws/from-host.txt")
        # Postgres on the claimed workspace, as a service would run.
        row["postgres"] = run(N, "cd /slot/ws && (setsid nohup ./scripts/postgres.sh >/tmp/pg.log 2>&1 &);"
                                 "for i in $(seq 50); do pg_isready -q && break; sleep 0.1; done;"
                                 "psql -Atc 'select count(*) from pg_class'")
        run(N, "pg_ctl stop -D /slot/ws/.fern/pg -m fast >/dev/null 2>&1")
        # Swap the workspace while the box runs (release + claim another).
        box.sh("umount", "-l", f"{slot}/ws")
        row["after_unmount"] = run(N, "ls -A /slot/ws | wc -l")
    # Mounting over the slot's root itself, which the box's gofer opened.
    box.sh("mount", "--bind", claim, slot)
    row["over_root"] = run(N, "ls -A /slot | awk 'NR<=4'")
    print(json.dumps(row), flush=True)
    box.destroy(bid)
    box.net_down(N)
    for _ in range(4):
        subprocess.run(["umount", "-l", f"{slot}/ws"], capture_output=True)
        subprocess.run(["umount", "-l", f"{slot}/late"], capture_output=True)
        subprocess.run(["umount", "-l", slot], capture_output=True)
    box.sh("btrfs", "subvolume", "delete", claim)


ANN = "io.kubernetes.cri.container-type", "io.kubernetes.cri.sandbox-id"


def warm(sid, n):
    """A warm sandbox: a root container that only sleeps, like a pod's
    pause container, with no workspace."""
    empty = f"{S}/ws/empty"
    os.makedirs(empty, exist_ok=True)
    b = box.bundle(sid, n, empty)
    spec = json.load(open(f"{b}/config.json"))
    spec["process"]["args"] = ["/bin/sleep", "infinity"]
    spec["mounts"] = [m for m in spec["mounts"] if m["destination"] not in ("/tmp", empty)]
    spec["annotations"] = {ANN[0]: "sandbox", ANN[1]: sid}
    json.dump(spec, open(f"{b}/config.json", "w"))
    box.create_start(sid, b)


def claim(sid, cid, n, ws):
    """Adds fern-init with the thread's workspace to the warm sandbox."""
    b = box.bundle(cid, n, ws)
    spec = json.load(open(f"{b}/config.json"))
    spec["annotations"] = {ANN[0]: "container", ANN[1]: sid}
    spec["linux"]["cgroupsPath"] = f"/spike/{cid}"
    json.dump(spec, open(f"{b}/config.json", "w"))
    box.create_start(cid, b)


def subcontainer(count):
    from bench import Total  # same CPU accounting as the benchmark
    for i in range(count):
        sid, cid, ws = f"warm{i}", f"claim{i}", f"{S}/ws/claim{i}"
        box.snapshot(f"{S}/golden", ws)
        box.net_up(N)
        with Total() as tot:
            warm(sid, N)
            warmed = tot.lap()
        time.sleep(1.0)
        row = {"case": "subcontainer", "i": i, "warm": warmed, "mem_warm": box.mem(sid)}
        with Total() as tot:
            claim(sid, cid, N, ws)
            row["started"] = tot.lap()
            box.wait_ready(N)
            row["ready"] = tot.lap()
        r = box.bexec(N, "sh", "-c", "pwd; psql -Atc 'select count(*) from pg_class'; echo hi > from-box.txt")
        row["exec_ok"] = r.returncode == 0 and os.path.exists(f"{ws}/from-box.txt")
        row["mem_claimed"] = box.mem(sid)
        print(json.dumps(row), flush=True)
        with open(f"{S}/results/subcontainer.jsonl", "a") as f:
            f.write(json.dumps(row) + "\n")
        box.runsc("kill", cid, "TERM", check=False)
        box.runsc("wait", cid, check=False)
        box.destroy(cid)
        box.destroy(sid)
        box.net_down(N)
        box.sh("btrfs", "subvolume", "delete", ws)


def main():
    box.cg_setup()
    os.makedirs(f"{S}/results", exist_ok=True)
    if sys.argv[1] == "propagate":
        propagate(sys.argv[2])
    else:
        subcontainer(int(sys.argv[2]))


if __name__ == "__main__":
    main()
