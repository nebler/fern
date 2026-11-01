#!/usr/bin/env python3
"""Runs Fern-like boxes with plain runsc, outside Kubernetes (spike).

Runs on the node as root. Everything lives under /var/lib/fern/spike (a btrfs
subvolume), /run/spike-runsc (runsc's state, apart from k3s's), netns
spike-<n> and the cgroup /sys/fs/cgroup/spike. Nothing of Fern's or k3s's is
touched.
"""
import json
import os
import resource
import subprocess
import sys
import time
import urllib.request

S = "/var/lib/fern/spike"
ROOTFS = f"{S}/rootfs"
ROOT = "/run/spike-runsc"
CG = "/sys/fs/cgroup/spike"
TOKEN = "spike-token"
UID = GID = 1000
# k3s's runsc shim has no options file (config-v3.toml.d/runsc.toml only
# names the runtime type), so its boxes run with runsc's defaults:
# platform systrap, network sandbox, overlay2 root:self, directfs,
# file-access exclusive (root) / shared (mounts). Extra flags come from
# RUNSC_FLAGS.
RUNSC = ["runsc", f"--root={ROOT}"] + os.environ.get("RUNSC_FLAGS", "").split()


def sh(*args, check=True, capture=True, **kw):
    r = subprocess.run(args, capture_output=capture, text=True, **kw)
    if check and r.returncode != 0:
        raise RuntimeError(f"{' '.join(args)}: {r.returncode}: {(r.stderr or '').strip()}")
    return r


def runsc(*args, check=True, **kw):
    return sh(*RUNSC, *args, check=check, **kw)


# --- cgroups -----------------------------------------------------------------

def cg_setup():
    os.makedirs(CG, exist_ok=True)
    with open(f"{CG}/cgroup.subtree_control", "w") as f:
        f.write("+cpu +memory +pids")
    os.makedirs(f"{CG}/cli", exist_ok=True)
    # The harness and every runsc CLI it starts run here; runsc moves the
    # sandbox and gofer into the box's own cgroup (cgroupsPath).
    with open(f"{CG}/cli/cgroup.procs", "w") as f:
        f.write(str(os.getpid()))


def cpu_usec(cg):
    try:
        with open(f"{CG}/{cg}/cpu.stat") as f:
            for line in f:
                k, v = line.split()
                if k == "usage_usec":
                    return int(v)
    except FileNotFoundError:
        return 0
    return 0


def self_usec():
    r = resource.getrusage(resource.RUSAGE_SELF)
    return int((r.ru_utime + r.ru_stime) * 1e6)


class CPU:
    """CPU used by the runsc CLIs (cgroup cli minus this harness) and by
    the box's own cgroup (sandbox, gofer) between start() and stop()."""

    def __init__(self, box):
        self.box = box

    def __enter__(self):
        self.cli0, self.self0, self.box0 = cpu_usec("cli"), self_usec(), cpu_usec(self.box)
        self.t0 = time.perf_counter()
        return self

    def __exit__(self, *exc):
        self.wall_ms = (time.perf_counter() - self.t0) * 1000
        self.cli_ms = ((cpu_usec("cli") - self.cli0) - (self_usec() - self.self0)) / 1000
        self.box_ms = (cpu_usec(self.box) - self.box0) / 1000
        self.cpu_ms = self.cli_ms + self.box_ms
        return False


# --- network -----------------------------------------------------------------

def net_up(n):
    """A netns with a veth like a CNI's: the box is 10.250.<n>.2."""
    ns = f"spike-{n}"
    if os.path.exists(f"/run/netns/{ns}"):
        net_down(n)
    sh("ip", "netns", "add", ns)
    sh("ip", "link", "add", f"spk{n}h", "type", "veth", "peer", "name", "eth0", "netns", ns)
    sh("ip", "addr", "add", f"10.250.{n}.1/24", "dev", f"spk{n}h")
    sh("ip", "link", "set", f"spk{n}h", "up")
    sh("ip", "-n", ns, "addr", "add", f"10.250.{n}.2/24", "dev", "eth0")
    sh("ip", "-n", ns, "link", "set", "eth0", "up")
    sh("ip", "-n", ns, "link", "set", "lo", "up")
    sh("ip", "-n", ns, "route", "add", "default", "via", f"10.250.{n}.1")
    return f"/run/netns/{ns}"


def net_down(n):
    sh("ip", "link", "del", f"spk{n}h", check=False)
    sh("ip", "netns", "del", f"spike-{n}", check=False)


def box_ip(n):
    return f"10.250.{n}.2"


# --- bundles -----------------------------------------------------------------

def bundle(bid, n, workspace, extra_mounts=(), fresh_tmp=True):
    """Writes the OCI bundle of box bid, like Fern's pod: fern-init as uid
    1000 with no capabilities, a read-only root, /tmp from the host (the
    pod's emptyDir) and the workspace at its own path."""
    b = f"{S}/bundle/{bid}"
    os.makedirs(b, exist_ok=True)
    tmp = f"{S}/tmp/{bid}"
    # A fresh box gets an empty /tmp, as a pod gets a new emptyDir; a
    # restored one needs the /tmp it was checkpointed with.
    if fresh_tmp and os.path.exists(tmp):
        sh("rm", "-rf", tmp)
    os.makedirs(tmp, exist_ok=True)
    os.chown(tmp, UID, GID)
    mounts = [
        {"destination": "/proc", "type": "proc", "source": "proc"},
        {"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "strictatime", "mode=755", "size=65536k"]},
        {"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": ["nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"]},
        {"destination": "/dev/shm", "type": "tmpfs", "source": "shm", "options": ["nosuid", "noexec", "nodev", "mode=1777", "size=65536k"]},
        {"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": ["nosuid", "noexec", "nodev", "ro"]},
        {"destination": "/tmp", "type": "bind", "source": tmp, "options": ["rbind", "rw"]},
        {"destination": workspace, "type": "bind", "source": workspace, "options": ["rbind", "rw"]},
    ] + list(extra_mounts)
    spec = {
        "ociVersion": "1.2.0",
        "process": {
            "user": {"uid": UID, "gid": GID},
            "args": ["/usr/local/bin/fern-init"],
            "env": [
                "PATH=/usr/lib/postgresql/17/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                "HOME=/tmp", "PGHOST=/tmp", "PGUSER=postgres", "PGDATABASE=postgres", "DISABLE_AUTOUPDATER=1",
                f"FERN_BOX_TOKEN={TOKEN}", f"FERN_POD_UID={bid}", f"FERN_WORKSPACE={workspace}",
            ],
            "cwd": "/",
            "noNewPrivileges": True,
            "capabilities": {k: [] for k in ("bounding", "effective", "inheritable", "permitted", "ambient")},
            "rlimits": [{"type": "RLIMIT_NOFILE", "hard": 1048576, "soft": 1048576}],
        },
        "root": {"path": ROOTFS, "readonly": True},
        "hostname": bid,
        "mounts": mounts,
        "linux": {
            "cgroupsPath": f"/spike/{bid}",
            # The pod's limits: 2 CPUs, 2 GiB.
            "resources": {"cpu": {"quota": 200000, "period": 100000}, "memory": {"limit": 2 << 30}},
            "namespaces": [{"type": "pid"}, {"type": "network", "path": f"/run/netns/spike-{n}"},
                           {"type": "ipc"}, {"type": "uts"}, {"type": "mount"}],
        },
    }
    with open(f"{b}/config.json", "w") as f:
        json.dump(spec, f, indent=1)
    return b


# --- readiness and exec ------------------------------------------------------

def wait_ready(n, timeout=30.0):
    """Polls /readyz?wait=10s like the daemon until it answers 200."""
    url = f"http://{box_ip(n)}:7070/readyz?wait=10s"
    deadline = time.monotonic() + timeout
    while True:
        try:
            with urllib.request.urlopen(url, timeout=11) as r:
                if r.status == 200:
                    return
        except Exception:
            if time.monotonic() > deadline:
                raise
            time.sleep(0.005)


def bexec(n, *argv):
    r = sh(f"{S}/probe", f"ws://{box_ip(n)}:7070", TOKEN, *argv, check=False)
    return r


# --- lifecycle ---------------------------------------------------------------

def logf(bid):
    os.makedirs(f"{S}/logs", exist_ok=True)
    return open(f"{S}/logs/{bid}.log", "a")


def create_start(bid, b):
    # The container's stdio is create's (start's for a subcontainer): a
    # pipe here would never reach EOF.
    with logf(bid) as log:
        for args in (("create", f"--bundle={b}", bid), ("start", bid)):
            sh(*RUNSC, *args, capture=False, stdin=subprocess.DEVNULL, stdout=log, stderr=log)


def destroy(bid):
    runsc("delete", "-force", bid, check=False)
    try:
        os.rmdir(f"{CG}/{bid}")
    except OSError:
        pass


def procs(bid):
    """PIDs of the box's sandbox and gofer (the cgroup's processes)."""
    try:
        with open(f"{CG}/{bid}/cgroup.procs") as f:
            return [int(x) for x in f.read().split()]
    except FileNotFoundError:
        return []


def mem(bid):
    """RSS and PSS of the box's processes (kB), and the cgroup's memory."""
    rss = pss = anon = 0
    for pid in procs(bid):
        try:
            with open(f"/proc/{pid}/smaps_rollup") as f:
                for line in f:
                    p = line.split()
                    if p[0] == "Rss:":
                        rss += int(p[1])
                    elif p[0] == "Pss:":
                        pss += int(p[1])
                    elif p[0] == "Anonymous:":
                        anon += int(p[1])
        except FileNotFoundError:
            pass
    cur = int(open(f"{CG}/{bid}/memory.current").read()) // 1024
    stat = dict(l.split() for l in open(f"{CG}/{bid}/memory.stat"))
    return {"rss_kb": rss, "pss_kb": pss, "anon_kb": anon, "cg_kb": cur,
            "cg_anon_kb": int(stat["anon"]) // 1024, "cg_file_kb": int(stat["file"]) // 1024,
            "cg_shmem_kb": int(stat["shmem"]) // 1024}


def du_kb(path):
    return int(sh("du", "-sk", "--apparent-size", path).stdout.split()[0]), int(sh("du", "-sk", path).stdout.split()[0])


def snapshot(src, dst):
    if os.path.exists(dst):
        sh("btrfs", "subvolume", "delete", dst)
    sh("btrfs", "subvolume", "snapshot", src, dst)


def stop(bid, n):
    """Fern's sleep: an authenticated POST /stop, then wait for the sandbox
    to exit and delete it."""
    req = urllib.request.Request(f"http://{box_ip(n)}:7070/stop", method="POST",
                                 headers={"Authorization": f"Bearer {TOKEN}", "If-Match": json.dumps(bid)})
    with urllib.request.urlopen(req, timeout=5) as r:
        assert r.status == 204, r.status
    runsc("wait", bid)
    destroy(bid)


def total_usec():
    """CPU of everything under the spike cgroup (CLIs, sandboxes, gofers)."""
    return cpu_usec(".")
