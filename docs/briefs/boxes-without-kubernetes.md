# Boxes without Kubernetes: the daemon runs them (Phase 3A)

Decided 2026-10-27 after the runsc spike (`lab/spike/runsc/RESULTS.md`).
Kubernetes keeps running the node, the daemon, T3, the Claude login Secret and
ingress; it stops starting and stopping boxes. This changes "Kubernetes only"
in AGENTS.md and architecture §10, §13 and §14 for boxes; those are updated as
each step lands.

## Why

| On the VPS, p50 | Kubernetes pod (today) | runsc, started by the daemon (spike) |
|---|---|---|
| wake, wall | 1,030–1,100 ms | 494 ms |
| wake, CPU | ~1,600 ms (node) | 683 ms |
| sleep, wall | 1,200–1,600 ms | 258 ms |

The pod path pays for the API server, the controller, kubelet's volume and
PLEG loops, the CSI calls and the CNI; a box needs none of them. Checkpoint and
restore were measured and rejected: 362 ms of CPU, they can't serve a revert
(the image fits only the disk it was taken with), and the image holds the box's
credentials.

## What the daemon does instead

A second driver, `internal/runsc`, behind the actor's `Driver` interface (Wake,
Sleep, Delete, States). The daemon picks it with `--boxes=runsc`; Kubernetes
stays the default until the gate and `check.sh all` pass with runsc.

- **Root filesystem:** the box image's root is in the daemon's image
  (`/opt/fern-box`, a build stage), copied once per content hash to
  `/var/lib/fern/images/<hash>` and used read-only, with runsc's in-memory
  overlay for writes (as today: `overlay2=root:self`).
- **Bundle:** an OCI spec per wake in `envs/<id>/box/`, the pod template's
  equivalent: fern-init as uid 1000, no capabilities, `noNewPrivileges`, a
  read-only root, the same CPU, memory and pid limits; the workspace, Claude's
  folder and the git folder (read-only) bound at their own paths, as the CSI
  driver mounts them now; `/tmp` from `envs/<id>/box/tmp`, emptied at every wake.
- **Process:** `runsc create` and `start` with `--root /run/fern/runsc`, each box
  in its own cgroup under `/sys/fs/cgroup/fern/`. The daemon runs with the host's
  PID and network namespaces, so boxes outlive a daemon restart and it can reach
  them.
- **Network:** a netns per wake (gVisor's netstack takes the interface's
  addresses at boot, so it can't be reused), a veth to the host with a /30 from
  `10.89.0.0/16`, and an nftables table in the host's namespace that gives a box
  the internet and nothing else: no private ranges, link-local, the node's own
  addresses or other boxes. Only the daemon connects to a box, on 7070. DNS comes
  from public resolvers (`--box-dns`), as the node's resolver is the node.
- **Login:** the daemon reads the `claude-login` Secret and passes it in the
  box's environment, as kubelet does now; never written to a file.
- **Stop:** `POST /stop` to fern-init (as now), `runsc wait`, then `runsc delete`
  and the netns. Crash-safe: the disk says what was under way: a box's directory
  holds its bundle while it may run, and `Reconcile` at start compares
  `runsc list` with it, adopting running boxes and deleting half-made ones.
- **What goes away for boxes:** the `Sandbox` objects and agent-sandbox, the CSI
  driver's box mounts, the pod network policy (replaced by nftables), and
  kubelet's PLEG wait.

## Steps

1. The runsc driver: wake, exec, sleep, delete, states, in the local lab
   (`lab/up.sh` runs k3s in Docker; runsc runs there already). Unit tests for the
   bundle and network plan; the lab for the rest.
2. Network isolation and its checks (`check.sh slice`'s network checks against
   nftables instead of the network policy).
3. Crash safety: `Reconcile`, a daemon restart with boxes awake, a crash at
   each step of wake and stop.
4. The gate and `check.sh all` on the VPS with `--boxes=runsc`; A/B against
   Kubernetes boxes.
5. Make runsc the default; remove agent-sandbox, the box CSI mounts and the
   network policy from the install.

A warm pool (a ready sandbox that a wake claims as a subcontainer, 395 ms and
503 ms of CPU in the spike) can come after step 5.
