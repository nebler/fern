# Restore without stopping the pod

**Verdict: reject the drop-in change.** A parent mount sees the exchange, but
today's CSI mount stays on the old disk, and replacing a parent-mounted
workspace bypasses T3's read-only `.git` protection. Keep pod-stop restore;
a surviving pod needs a revised storage, security and recovery contract.

Measured locally on 2026-10-25 from `fc7dd98b`: ARM64 Docker Desktop,
gVisor `release-20261012.0`. Every build and measurement held `lab_lock`.
No VPS, performance-baseline edits or production restore changes.

## Mount and process evidence

CSI `NodePublishVolume` binds `Env.Workspace` itself at the same path T3
uses: `envs/<id>/live` normally, `worktrees/<repo>/<name>` for T3.
The gofer/directfs mount retains that directory, independently of its name.

- **Current CSI box:** stop Postgres cleanly, snapshot the restore target
  into `live.new`, exchange it with `live`, then restart Postgres without
  stopping the pod. Host marker: `restored`; fresh box read: `current`.
  Postgres still has **4 rows**. A real sleep/wake mounts the restored disk
  and gives **3 rows**. Service restart alone cannot retarget the mount.
- **Parent-bind probe:** an isolated gVisor pod mounted the environment
  parent using hostPath, plus a direct bind and a read-only `.git` subPath
  matching the driver's protection. Over **100 exchanges**, fresh absolute
  reads and primed positive/negative lookups had **zero errors**. Retained
  cwd, an open file descriptor, and the direct bind kept reading `old`.
  Exchange command p50 **1.03 ms**; host-to-guest verification p50 **5.52 ms**.
  These are primitive timings, not Fern restores.
- **Security failure:** `.git` returned `EROFS` on all 50 visits to the
  original tree, but opening it for writing succeeded on **all 50 visits
  to the replacement**. The subPath mount protects the old file. This
  breaks architecture §13's protection against a box redirecting T3's Git.

The runtime configuration had no overrides and used the native runsc binary.
Defaults: `file-access=exclusive` for the image root,
`file-access-mounts=shared`, `directfs=true`, `overlay2=root:self`.
The guest mount showed `cache=remote_revalidating,directfs`; workspace writes
reach btrfs without an overlay. See the
[pinned defaults](https://github.com/google/gvisor/blob/release-20261012.0/runsc/config/flags.go)
and [revalidation code](https://github.com/google/gvisor/blob/release-20261012.0/pkg/sentry/fsimpl/gofer/revalidate.go).
The shared cache works; retained process references are the other problem.

## Timings

The prescribed build, `REPEAT=3 lab/perf.sh t3`, and
`lab/check.sh restore t3 box` passed under one lock:

- `t3.restore_ms` median: **619.5 ms**.
- Separate Postgres box wake: p50 **683.3 ms**, p95 **902.5 ms**, n=10.

A second build plus `lab/t3-run.sh toy 3` under one lock:
**3/3 passed**, 110 s for the scenarios.

- T3 revert wall time: **2,190 ms p50**, **2,652 ms p95**.
- Fern restore within those reverts: **847 ms p50**.
- Next-turn wake: **893 ms p50**, **1,371 ms p95**.
- Raw revert / Fern restore / following wake triples, ms:
  `2652/1432/878`, `1591/836/893`, `2190/847/1371`.

Pod stop consumed 815–1,416 ms; the rest of Fern's restore took 16–21 ms.
T3 also spent 755–1,343 ms outside Fern's restore. A 100–300 ms engine
restore alone would not make the full T3 revert that fast. No valid T3 B
exists here: the parent variant fails isolation before a performance gate.

An additional manual parent/Postgres trial stopped because its fixture lacked
the `amcheck` extension. The corrected repeat was canceled while still queued
after the isolation failure settled the verdict. Parent/Postgres latency and
full consistency validation are therefore **not established** by this probe.

## Proposal and best alternative

Keeping the pod changes architecture §§4, 7.3, 10, 12 and 13. Mounting
`envs/<id>` exposes snapshots and staging; mounting a T3 workspace's
parent exposes sibling threads. First design a private, stable export
preserving T3's paths, with Git protection independent of replaced inodes.

Then add an authenticated quiesce/reload protocol to fern-init: gate execs
and readiness, stop supervisors, kill and await agent processes including
`setsid` descendants, and reload the restored services. Check a generation
inside fern-init so an old exec cannot connect after admission reopens.
Stop Postgres before swapping and restart it afterwards; a checkpoint alone
does not reset its open files or memory.
[PostgreSQL 17 restore requirements](https://www.postgresql.org/docs/17/backup-file.html).

Persist intent and prepare/quiesced/swapped/resumed phases outside the
workspace. Reconcile must finish or abort the handshake before services
resume, including lost acknowledgements and daemon/fern-init restarts.
Today's cleanup of `live.new` cannot do that. Add failpoints for every new
disk/control transition and delayed exec admission.

The smaller alternative is **pre-wake after T3 completes its Git restore**.
It could hide the next-turn wake during user think time; it does not shorten
restore itself. Waking from the current pre-Git hook would race T3's writes.
Since built, after T3's whole rollback rather than its restore: see
[architecture §6.4](../docs/architecture.md#64-pre-wake).

Validation passed: `lab/go.sh test -race ./...`, `lab/lint.sh` (0 issues),
and `lab/test-store.sh -run '^TestCrash/restore$' -v` (all five existing
restore failpoints on real btrfs). These validate the unchanged restore,
not a new live-restore protocol.
