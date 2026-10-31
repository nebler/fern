#!/usr/bin/env python3
"""Spike: do file events cross from Claude's container to the services'
when both are containers in one gVisor sandbox? Runs on the node as root.

  watch.py MODE   MODE is how the workspace is mounted in the two containers:
                  two-sandboxes  today's split: a sandbox each, a bind each
                  binds          one sandbox, a plain bind in each container
                  pod            one sandbox, a pod-shared mount (share=pod)
                  shared         one sandbox, share=shared (the host may write)

The services' container runs a watcher (fs.watch, as Vite's chokidar does)
on src/; the agent's container changes, adds, deletes and renames files
there, then the host writes one. Prints which events the watcher saw.
"""
import json
import os
import shutil
import subprocess
import sys
import time

S = "/var/lib/fern/spike-watch"
ROOTFS = "/var/lib/fern/images/fern-box:dev/root"
RUNSC = ["runsc", "--root=/run/spike-watch", "--network=none", "--ignore-cgroups"] + os.environ.get("RUNSC_FLAGS", "").split()
W = f"{S}/ws"
TYPE, SID = "io.kubernetes.cri.container-type", "io.kubernetes.cri.sandbox-id"
WATCHER = """
import fs from "node:fs";
fs.watch("src", (e, f) => fs.appendFileSync("events.log", `${e} ${f}\\n`));
fs.writeFileSync("events.log", "watching\\n");
setInterval(() => {}, 1e6);
"""


def sh(*args, check=True, **kw):
    r = subprocess.run(args, capture_output=True, text=True, **kw)
    if check and r.returncode != 0:
        raise RuntimeError(f"{' '.join(args)}: {r.returncode}: {r.stderr.strip()}")
    return r


def spec(cid, args, mode, sandbox):
    mounts = [
        {"destination": "/proc", "type": "proc", "source": "proc"},
        {"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "mode=755"]},
        {"destination": "/tmp", "type": "tmpfs", "source": "tmpfs", "options": ["mode=1777"]},
        {"destination": W, "type": "bind", "source": W, "options": ["rbind", "rw"]},
    ]
    ann = {}
    if mode != "two-sandboxes":
        ann = {TYPE: "container", SID: sandbox}
        if cid == sandbox:
            ann[TYPE] = "sandbox"
        if mode in ("pod", "shared"):
            # The pod's annotations, which every container carries, as
            # containerd's CRI passes them: each container's bind of the
            # same source then gets the one mount.
            p = "dev.gvisor.spec.mount.ws."
            ann |= {p + "source": W, p + "type": "bind", p + "share": mode, p + "options": "rw"}
    return {
        "ociVersion": "1.2.0",
        "process": {"user": {"uid": 1000, "gid": 1000}, "args": args, "cwd": W,
                    "env": ["PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp"]},
        "root": {"path": ROOTFS, "readonly": True},
        "hostname": cid,
        "mounts": mounts,
        "annotations": ann,
        "linux": {"namespaces": [{"type": t} for t in ("pid", "ipc", "uts", "mount", "network")]},
    }


def start(cid, s):
    b = f"{S}/bundle/{cid}"
    os.makedirs(b, exist_ok=True)
    json.dump(s, open(f"{b}/config.json", "w"))
    with open(f"{S}/{cid}.log", "w") as log:
        for args in (("create", f"--bundle={b}", cid), ("start", cid)):
            subprocess.run(RUNSC + list(args), stdin=subprocess.DEVNULL, stdout=log, stderr=log, check=True)


def rexec(cid, script):
    return sh(*RUNSC, "exec", "--user=1000:1000", "--cwd", W, cid, "sh", "-c", script)


def main():
    mode = sys.argv[1]
    cleanup()
    os.makedirs(f"{W}/src", exist_ok=True)
    open(f"{W}/w.mjs", "w").write(WATCHER)
    for f in ("a.txt", "del.txt", "old.txt"):
        open(f"{W}/src/{f}", "w").write("x\n")
    sh("chown", "-R", "1000:1000", S)
    svc, agent = "svc", "agent"
    if mode == "two-sandboxes":
        start(svc, spec(svc, ["node", "w.mjs"], mode, None))
        start(agent, spec(agent, ["sleep", "infinity"], mode, None))
    else:
        # The sandbox's own container is the agent's; the services join it.
        start(agent, spec(agent, ["sleep", "infinity"], mode, agent))
        start(svc, spec(svc, ["node", "w.mjs"], mode, agent))
    for _ in range(50):
        if os.path.exists(f"{W}/events.log"):
            break
        time.sleep(0.1)
    def lines():
        return [e for e in open(f"{W}/events.log").read().split("\n") if e][1:]
    rexec(svc, "echo own >> src/a.txt")
    time.sleep(0.5)
    own = lines()
    rexec(agent, "echo agent >> src/a.txt; echo n > src/new.txt; rm src/del.txt; mv src/old.txt src/renamed.txt")
    time.sleep(1)
    agent_ = lines()
    open(f"{W}/src/host.txt", "w").write("h\n")
    time.sleep(1)
    host = lines()
    seen = rexec(svc, "cat src/a.txt; ls src").stdout.split()
    print(json.dumps({
        "mode": mode,
        "own": own,
        "from_agent": agent_[len(own):],
        "from_host": host[len(agent_):],
        "svc_reads": seen,
    }))
    cleanup()


def cleanup():
    for cid in ("svc", "agent"):
        subprocess.run(RUNSC + ["delete", "-force", cid], capture_output=True)
    shutil.rmtree(S, ignore_errors=True)


if __name__ == "__main__":
    main()
