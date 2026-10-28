# Fern without Kubernetes

**Status:** building (2026-10-28). **Decision:** on one server, Fern runs as two systemd services, without k3s. Installing into an existing Kubernetes cluster stays possible with `deploy/fern.yaml`, but it is no longer the default or what the lab tests.

## Why

Since the daemon runs boxes itself ([boxes-without-kubernetes.md](boxes-without-kubernetes.md)), in the default mode Kubernetes only:

- keeps two programs running (the daemon and T3's server);
- stores Claude's login as a Secret;
- installs them with `kubectl apply`.

Measured on the VPS (`lab/perf/vps.tsv`, commit `81fb73b2`), it costs this while idle, with k3s's extras already off (traefik, servicelb, metrics-server, local-storage):

| | memory (RSS) | CPU while idle |
|---|---|---|
| k3s (API server, kubelet, controllers, kube-router, CoreDNS) | 607 MiB | 4.1 s/min (7% of a core) |
| containerd | 170 MiB | 1.3 s/min (2%) |
| **together** | **~780 MiB** | **~9% of a core** |

The whole node idles at 10 s/min, so Kubernetes is about half of the node's idle CPU. A box is 45 MiB, so the same memory holds about 17 more awake boxes.

systemd does the same three jobs, and it is already running.

## What changes

| Job | With k3s | Without |
|---|---|---|
| Keep the daemon running | DaemonSet (privileged, hostPID, hostNetwork) | `fern-daemon.service`, on the host |
| Keep T3 running | Deployment, NodePort 30773 | `fern-t3.service`: its image's root as `RootDirectory=`, as the box user, port 3773 |
| Claude's login | Secret `claude-login` | `/etc/fern/claude.env`, root-only (0600), read at every wake; T3 gets it as `EnvironmentFile=` |
| Images (the box's, T3's) | containerd's store, `ctr images mount` | `fern image load NAME`: a `docker save` stream unpacked into `<pool>/images/<id>/`, `<pool>/images/NAME` pointing at the current one. The ID is the config's digest, as containerd's was, so golden keys stay the same |
| Programs on the node | `nsenter` into PID 1's namespaces | run directly: the daemon detects that it is in the host's mount namespace |
| CSI | serves boxes' volumes with `--boxes=kubernetes` | not started unless `--boxes=kubernetes` |

Nothing changes for boxes, the store, snapshots, the API or T3's patches.

## The lab

The local lab becomes one privileged Docker container with systemd as PID 1, the btrfs pool and runsc: the same units as the VPS. `on_node` replaces `kubectl exec`, and `journalctl -u fern-daemon` replaces `kubectl logs`. `lab/vps.sh` installs the units; on a VPS that had k3s, it removes k3s first. The pool (`/var/lib/fern`) stays.

## Kept for clusters

`deploy/fern.yaml` (runsc boxes in a privileged DaemonSet), the agent-sandbox driver (`--boxes=kubernetes`) and the CSI plugin stay in the repository for people who already run a cluster. The lab's suite doesn't test them any more.

## Done when

- `lab/check.sh all`, the perf gate and `lab/t3-run.sh` pass on the VPS without k3s.
- The VPS's new baseline shows the idle memory and CPU above gone.
