# Spike: boxes under plain runsc, checkpoint/restore and a warm pool

**Question:** should the daemon run boxes itself, with gVisor checkpoint/restore,
instead of starting pods through Kubernetes? The bar was a restore costing about
100 ms of CPU and a few hundred ms wall.

**Answer: not for checkpoint/restore (option a fails).** A restore costs
**362 ms CPU and 222 ms wall** to a ready box (p50, page cache warm), and
542 ms CPU / 535 ms wall with a cold cache. That is well above the bar. It also
can't serve a revert, and its image would hold the box's credentials. What does
move the numbers is **leaving Kubernetes**: a plain runsc cold start of the same
box is **494 ms wall / 683 ms CPU**, against today's 1.03–1.10 s / ~1,680 ms. A
warm pool of sandboxes (#7) works cleanly through subcontainers and saves another
~100 ms wall / ~180 ms CPU. Recommendation below.

Measured 2026-10-27 on the VPS (Ubuntu 26.04, kernel 7.0.0-30, arm64, 4 CPUs,
7.7 GB). The lab lock was held throughout, k3s and Fern's folders were not
touched, and everything was cleaned up afterwards.

## Method

- **runsc:** `release-20261012.0` (spec 1.2.1), `/usr/local/bin/runsc`. k3s's
  shim has no options file (`config-v3.toml.d/runsc.toml` only sets
  `runtime_type = "io.containerd.runsc.v1"`), so its boxes run with runsc's
  defaults. The spike used the same defaults: `--platform=systrap`, netstack
  (`--network=sandbox`), `--overlay2=root:self`, `--directfs=true`,
  `--file-access=exclusive` for the root and `shared` for mounts. The only
  flag added was `--root=/run/spike-runsc`, which keeps the spike's state
  apart from k3s's. The guest mounts showed
  `cache=remote_revalidating,directfs`, the same as in the pods.
- **Box:** an OCI bundle built from `fern-box:dev`'s root filesystem
  (`ctr images mount`, then copied). It mirrors the driver's pod spec:
  `fern-init` as uid/gid 1000, no capabilities, `noNewPrivileges`, a read-only
  root, 2 CPUs and 2 GiB, `/tmp` from a host directory (the emptyDir), and the
  workspace bound at its own path. The workspace is a fresh btrfs snapshot of a
  golden subvolume (`lab/sample-repo`, seeded once), like a new thread.
  Postgres 17 runs from `services.yaml`.
- **Network:** a netns per box with a veth pair (`10.250.<n>.2`), like a CNI's.
  The harness dials fern-init's `:7070` over it: `/readyz?wait=10s` the way
  the daemon polls it, and exec over real boxexec (`probe/`, built with
  `-tags spike` from `internal/boxexec`).
- **CPU:** cgroup v2 `usage_usec` of `/sys/fs/cgroup/spike`. It holds the
  runsc CLIs (`spike/cli`) and each box's cgroup (`cgroupsPath`: sandbox,
  gofer, systrap stubs). The harness's own CPU (`getrusage`) is subtracted.
  netns setup is timed separately and is not in the CPU figures.
- **Memory:** PSS/RSS from `smaps_rollup` of every process in the box's
  cgroup, plus the cgroup's `memory.current` and `memory.stat`. gVisor keeps
  application memory in a memfd, which shows up as `shmem`.
- **"Cold cache"** means `sync; echo 3 > drop_caches` right before the
  operation. The pool is btrfs on a loop file, so the image is cached twice.
- n = 15 for each variant (9 for the 400 MB box, after dropping trial 0,
  where the 400 MB process never started). Percentiles are nearest-rank.
- Scripts: `run.sh` (holds `lab_lock`, copies everything, runs every phase,
  fetches `results/`, cleans up), `node-setup.sh`, `node-cleanup.sh`,
  `box.py` (bundle, netns, cgroup, lifecycle), `bench.py`, `correct.py`,
  `mount7.py`, `summarize.py` (the tables at the end).

## Headline numbers (p50 / p95)

| Operation | wall ms | CPU ms |
|---|---:|---:|
| **Today, Kubernetes wake** (for reference, from the profile) | 1,030–1,100 | ~1,680 |
| Cold start to `/readyz`, plain runsc | **494 / 522** | **683 / 729** |
| Cold start, cold cache | 789 / 842 | 756 / 801 |
| Warm sandbox, then claim (subcontainer) to `/readyz` | **395 / 439** | **503 / 549** |
| Restore to `/readyz` (13 MB image) | **222 / 239** | **362 / 394** |
| Restore, cold cache | 535 / 558 | 542 / 562 |
| Restore, `--background` | 225 / 250 | 365 / 407 |
| Restore, `--background`, cold cache | 532 / 566 | 542 / 571 |
| Restore, `--direct` image + restore | 222 / 240 | 365 / 383 |
| Restore, `flate-best-speed` image | 267 / 284 | 427 / 449 |
| Restore, `--exclude-committed-zero-pages` image | 223 / 235 | 368 / 391 |
| Restore, box with a 400 MB process (430 MB image) | 604 / 636 | 1,009 / 1,087 |
| ditto, `--background` | 526 / 544 | 974 / 1,061 |
| ditto, cold cache (with or without `--background`) | 847–852 / 886–902 | 1,117–1,155 / 1,173–1,193 |
| **Today, Kubernetes sleep** | 1,200–1,600 | — |
| Sleep, plain runsc (`POST /stop`, wait, delete) | **258 / 269** | 227 / 255 |
| Checkpoint (returns) | 94 / 105 | 139 / 158 |
| Checkpoint until the sandbox is gone | 124 / 137 | 193 / 209 |
| Checkpoint, `flate-best-speed` | 140 / 149 | 197 / 212 |
| Checkpoint, 400 MB box | 482 / 972 | 648 / 668 |
| netns + veth with `ip` commands (not included above) | 32 / 38 | — |

| Image on disk | MB |
|---|---:|
| Postgres box, plain / `--direct` | 13.0 / 12.9 |
| `--exclude-committed-zero-pages` | 12.6 |
| `flate-best-speed` | 1.6 |
| 400 MB random process | 429.5 |

| Memory (settled, 3 s after ready) | PSS MB | cgroup MB |
|---|---:|---:|
| Cold-started box | 132 | 39 |
| Restored box | 107 | 62 |
| Restored, flate image | 142 | 74 |
| Restored, cold cache | 109 | 197 (page cache of the image, charged to the box) |
| Idle warm sandbox (root container only) | 59 | 15 |
| Box with a 400 MB process, restored | 564 | 1,317 |

## What the numbers say

1. **Most of today's wake cost is Kubernetes, not gVisor.** The same box
   started by plain runsc needs 683 ms of CPU, against ~1,680 ms through
   kubelet, containerd, CNI and the pause container. Wall time halves, from
   1.05 s to 0.49 s. Sleep drops from 1.2–1.6 s to 258 ms, because nothing
   waits for PLEG. Caveat: the spike has no CNI plugin, NetworkPolicy, CSI
   publish or pause container, and the Kubernetes figures come from the
   earlier profile, not from this cgroup method. The gap is indicative, not
   exact.
2. **A restore is not ~100 ms of CPU.** The floor is the sandbox's own boot:
   a warm sandbox with no workload starts in 104 ms wall / 138 ms CPU. The
   debug timeline of one restore adds seccomp filter build (~16 ms), kernel
   state decode (~48 ms for a 400 KB `checkpoint.img`) and network/VFS
   restore (~2 ms). That brings restore's return to 155 ms wall / 221 ms CPU.
   The first readiness check then costs another ~70 ms wall / ~140 ms CPU:
   it forks `test` and `pg_isready` in a just-restored sentry. Against the
   plain cold start, restore saves ~270 ms wall and ~320 ms CPU when the
   cache is warm. With a cold cache it saves ~250 ms wall and ~210 ms CPU.
3. **Pages load eagerly.** In every variant, the box's full application
   memory sits in its memfd when `runsc restore` returns: 13.2 of 13.2 MB,
   and 429.7 of 429.7 MB for the big box. Nothing is demand-paged.
   `--background` only overlaps the copy with resuming the box: it saves
   ~80 ms wall on 430 MB, no CPU, and nothing with a cold cache. Restore
   cost therefore grows with the box's memory. A box running a 400 MB dev
   server or agent restores in 604 ms wall / 1,009 ms CPU. That is **worse
   than a cold start** (683 ms CPU), though the process inside is already
   running.
4. **The flags barely matter.** `--direct` and zero-page exclusion change
   nothing measurable. Compression shrinks the image 8x (13 MB to 1.6 MB),
   but costs +60 ms CPU per checkpoint, +65 ms CPU per restore and ~35 MB
   more PSS. Uncompressed images cost disk instead: 13 MB to 430 MB per
   asleep environment, on the pool.
5. **Checkpointing is a cheaper sleep than stopping the box**: 124 ms wall /
   193 ms CPU until the sandbox is gone, against 258 / 227 for
   `POST /stop`.

## Correctness

- **Postgres and pre-checkpoint files survive: 15/15 in every variant**
  (and 9/9 for the big box, whose process was still running). Every
  restore went onto the workspace **re-created from a snapshot at the same
  path**, so a new subvolume with new inode numbers. Restore tolerates a
  replaced mount source when its content matches.
- **Files changed on the host between checkpoint and restore**
  (`correct.py host-edit`): the box sees the edit, the deletion and the new
  file, its writes reach the subvolume, and Postgres answers. runsc doesn't
  validate or complain.
- **Open files are reopened by path.** A box process held `held.txt` and
  `replaced.txt` open across the checkpoint. After restore, its fd 3 read the
  host's append, and its fd 4 read the **new file** that had been renamed over
  `replaced.txt`. POSIX would keep the old inode. The change is silent.
- **A revert breaks it: the image fits only the exact disk it was taken
  with.** (`correct.py revert`) The workspace was snapshotted at turn A
  (1 row), changed to turn B (20,000 rows) and checkpointed. Then the
  subvolume was swapped for turn A's snapshot, as Fern's restore does, and
  the image restored onto it. `runsc restore` exited 0 and `/readyz`
  answered 200, but Postgres was inconsistent. `count(*)` returned 226, and
  an `INSERT` failed with `unexpected data beyond EOF in block 0 of relation
  base/5/16393_fsm`. A `CHECKPOINT` then wrote turn B's buffers onto turn A's
  files, and a later cold start found 20,000 rows: **the revert was silently
  undone**. Reverting to an earlier turn must cold-start, or restore an image
  taken at that same turn (a checkpoint per turn).
- **/tmp is part of the state.** The Postgres socket and its lock live there.
  A restored box needs the `/tmp` it was checkpointed with, and a fresh box
  needs an empty one. A stale `.s.PGSQL.5432.lock` kept a fresh Postgres
  from starting.
- **Network.** At boot the sentry takes the netns's addresses (eth0 is left
  without an IP), so every start and restore needs a freshly set-up netns.
  A restore into a netns with **a different IP works**: the box answered at
  the new address. TCP connections open at checkpoint time are dropped on
  restore (`TCP endpoint connect failed ... network is unreachable`), so
  exec sessions and terminals don't survive a checkpoint.
- **Credentials:** the image is the box's memory, so it holds
  `FERN_BOX_TOKEN` and, in a real box, `CLAUDE_CODE_OAUTH_TOKEN` (both are
  environment variables). Storing images on the pool would break "credentials
  never enter snapshots" in spirit, unless images are encrypted or kept off
  the pool.

## #7: a subvolume into an already running sandbox

**Yes, two ways.** The clean one is a subcontainer.

**Mount propagation** (`mount7.py propagate`): a host "slot" directory was made
a shared mount (`mount --bind S S; mount --make-shared S`) and bound into the
box at `/slot`. The subvolume was bind-mounted under it **after** the box was
running (2–3 ms).

| Bundle option | Result |
|---|---|
| `rshared` | runsc refuses: `mount option "rshared" is not supported` |
| `rslave` (directfs on, the default) | **works.** The box sees the subvolume, even in a directory it had already looked at while it was empty: the guest's `stat` changes from dev/ino `23 2` to `23 3`, so dentry revalidation catches it. Box writes land in the subvolume and host writes show in the box. Postgres runs on it (`select count(*) from pg_class` = 420). After a host unmount, the box sees the empty directory again |
| `rslave`, `--directfs=false` | works the same way |
| `rprivate` (the default) | the box keeps seeing the empty directory |
| mount over the slot's root itself | not visible: the gofer holds the original directory open |

Limits: the workspace appears under the slot, not at the path T3 uses. Getting
T3's path would mean binding the path's parent, which exposes sibling threads
(the isolation failure in `lab/LIVE-RESTORE.md`). The read-only `.git`
protection would have to be bound per claim too, and processes holding fds keep
the old tree.

**Subcontainer** (`mount7.py subcontainer`, how a pod's pause and app containers
already work): a warm sandbox runs a root container that only sleeps
(annotation `io.kubernetes.cri.container-type=sandbox`). Claiming it is
`runsc create` + `start` of fern-init as a second container
(`container-type=container`, `sandbox-id=<warm>`), with **the workspace mounted
at its real path** through its own gofer, so the normal mount rules apply.

| n=15 | p50 | p95 |
|---|---:|---:|
| warm sandbox start, done beforehand: wall / CPU ms | 104 / 138 | 129 / 159 |
| idle warm sandbox: PSS / cgroup MB | 59 / 15 | 60 / 16 |
| claim: subcontainer create+start, wall ms | 57 | 74 |
| claim to `/readyz`, wall ms | **395** | **439** |
| claim to `/readyz`, CPU ms | **503** | **549** |

15/15 claims served exec, Postgres and a workspace write. Note for later: a
subcontainer gets its stdio from `runsc start`, not `create`.

## Surprises

- Kubernetes itself is ~1 s of CPU and ~550 ms wall of every wake. That is
  far more than gVisor's share.
- Restore has no lazy page loading in this release, so its cost grows with the
  box's memory, and `--background` barely helps.
- Restore accepts a changed disk without a word, including one from a
  different point in time, and silently corrupts Postgres.
- Restored file descriptors follow the path, not the inode.
- `rshared` is rejected, but `rslave` is enough for propagation, in both
  directfs modes.
- With a cold cache, the image's page cache is charged to the box's cgroup
  (197 MB for a 13 MB image, read through btrfs on a loop file).
- After a cold-cache start, `POST /stop` took 3.16 s instead of 258 ms.
  Postgres was down in ~120 ms, so fern-init seems to have waited out
  `killDelay` (3 s) on some process. This is unrelated to runsc and wasn't
  investigated.

## Recommendation

**Not (a).** Restore is ~3.5x the 100 ms CPU bar, can't serve reverts, costs
more than a cold start for boxes with a few hundred MB of memory, ties every
asleep environment to an image of 13–430 MB, and puts credentials into that
image. Fern shouldn't build its wake on checkpoint/restore.

**Between (b) and (c), the deciding factor isn't gVisor at all.** If the goal is
wake cost, the lever is **the daemon starting boxes with runsc directly**:
494 ms / 683 ms CPU cold, and 395 ms / 503 ms CPU with a warm subcontainer
pool. Today's wake is ~1.05 s / ~1,680 ms. The warm pool adds only ~100 ms wall
/ ~180 ms CPU on top of direct runsc, at ~15 MB of memory per idle slot, so it
is optional. Within Kubernetes, neither is reachable. A running pod can only gain
ephemeral containers, and those can't add volumes. Kubernetes has no restore
API: its container checkpoint is forensic-only. That path is therefore **(c):
keep Kubernetes as is**.

**Main caveat:** AGENTS.md and architecture §10/§13 say Fern runs on Kubernetes
only, and boxes rely on Kubernetes for CSI mount authorization, NetworkPolicy,
admission (Sandbox pinned to the node) and the Sandbox's durable stop intent.
Running boxes from the daemon means rebuilding those: netns plus nftables
policy, mount authorization, crash-safe lifecycle intent on disk. It changes the
architecture, so it is a design decision, not a performance tweak. The
Kubernetes-versus-runsc gap here also compares different measurement methods
and a box without CNI or policy. Before deciding, measure a pod wake with the
same cgroup method and add nftables rules to the spike's netns.

## Full tables (`summarize.py results`)

### checkpoint plain (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| checkpoint returned, wall ms | 94.3 | 104.9 |
| checkpoint, CPU ms | 138.8 | 158.3 |
| sandbox exited + deleted, wall ms | 124.1 | 137.0 |
| sandbox exited + deleted, CPU ms | 192.8 | 209.0 |
| image on disk, MB | 13.0 | 13.1 |
| PSS before, MB | 133.8 | 135.0 |

### checkpoint flate (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| checkpoint returned, wall ms | 140.3 | 148.6 |
| checkpoint, CPU ms | 197.3 | 211.8 |
| sandbox exited + deleted, wall ms | 173.0 | 183.7 |
| sandbox exited + deleted, CPU ms | 248.0 | 268.6 |
| image on disk, MB | 1.6 | 1.6 |
| PSS before, MB | 133.3 | 134.5 |

### checkpoint direct (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| checkpoint returned, wall ms | 89.9 | 101.1 |
| checkpoint, CPU ms | 136.6 | 148.7 |
| sandbox exited + deleted, wall ms | 120.5 | 135.4 |
| sandbox exited + deleted, CPU ms | 189.0 | 204.0 |
| image on disk, MB | 12.9 | 13.0 |
| PSS before, MB | 133.7 | 135.1 |

### checkpoint zero (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| checkpoint returned, wall ms | 92.1 | 100.8 |
| checkpoint, CPU ms | 134.6 | 149.2 |
| sandbox exited + deleted, wall ms | 123.7 | 134.0 |
| sandbox exited + deleted, CPU ms | 185.7 | 200.8 |
| image on disk, MB | 12.6 | 12.7 |
| PSS before, MB | 133.7 | 134.7 |

### checkpoint big (n=9)

| metric | p50 | p95 |
|---|---:|---:|
| checkpoint returned, wall ms | 482.1 | 971.5 |
| checkpoint, CPU ms | 647.5 | 667.6 |
| sandbox exited + deleted, wall ms | 586.0 | 1,059.9 |
| sandbox exited + deleted, CPU ms | 767.4 | 796.8 |
| image on disk, MB | 429.5 | 429.6 |
| PSS before, MB | 268.4 | 273.2 |

### cold-cold-cache (n=12)

| metric | p50 | p95 |
|---|---:|---:|
| netns + veth setup, ms | 27.8 | 33.1 |
| create+start returned, wall ms | 310.7 | 334.4 |
| ready (/readyz 200), wall ms | 788.5 | 841.7 |
| ready, CPU ms (all) | 756.2 | 801.0 |
| ready, CPU ms (runsc CLIs) | 64.0 | 71.5 |
| CPU in the 3 s after ready, ms | 35.9 | 42.3 |
| first exec `true`, ms | 34.8 | 52.1 |
| exec `psql select 1`, ms | 54.4 | 73.8 |
| PSS at ready, MB | 128.2 | 141.9 |
| PSS settled, MB | 131.7 | 132.5 |
| cgroup memory settled, MB | 168.2 | 170.4 |
| sleep (POST /stop, wait, delete), wall ms | 3,160.1 | 3,183.6 |
| sleep, CPU ms | 311.4 | 349.8 |

### cold (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| netns + veth setup, ms | 31.9 | 37.5 |
| create+start returned, wall ms | 114.8 | 134.2 |
| ready (/readyz 200), wall ms | 493.7 | 521.9 |
| ready, CPU ms (all) | 682.9 | 728.6 |
| ready, CPU ms (runsc CLIs) | 38.2 | 47.4 |
| CPU in the 3 s after ready, ms | 47.0 | 57.1 |
| first exec `true`, ms | 15.5 | 21.2 |
| exec `psql select 1`, ms | 44.7 | 60.3 |
| PSS at ready, MB | 140.7 | 142.9 |
| PSS settled, MB | 132.4 | 133.4 |
| cgroup memory settled, MB | 39.4 | 48.1 |
| sleep (POST /stop, wait, delete), wall ms | 258.2 | 268.5 |
| sleep, CPU ms | 226.7 | 254.5 |

correct: 4 rows (not tabulated)

mount7: 3 rows (not tabulated)

### restore-background-cold (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 376.5 | 411.0 |
| restore returned, CPU ms | 312.1 | 323.4 |
| ready (/readyz 200), wall ms | 532.4 | 565.8 |
| ready, CPU ms (all) | 541.6 | 571.1 |
| ready, CPU ms (runsc CLIs) | 59.9 | 68.2 |
| CPU in the 3 s after ready, ms | 86.8 | 95.6 |
| first exec (cat + psql), ms | 100.7 | 133.6 |
| app memory loaded at restore return (shmem), MB | 13.2 | 13.2 |
| PSS at ready, MB | 121.9 | 123.4 |
| PSS settled, MB | 108.6 | 109.5 |
| cgroup memory settled, MB | 197.0 | 197.6 |

correct after restore: 15/15

### restore-background (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 155.0 | 173.4 |
| restore returned, CPU ms | 224.6 | 260.0 |
| ready (/readyz 200), wall ms | 225.1 | 249.7 |
| ready, CPU ms (all) | 365.2 | 407.2 |
| ready, CPU ms (runsc CLIs) | 25.5 | 29.4 |
| CPU in the 3 s after ready, ms | 49.1 | 76.7 |
| first exec (cat + psql), ms | 64.5 | 92.2 |
| app memory loaded at restore return (shmem), MB | 13.2 | 13.3 |
| PSS at ready, MB | 117.4 | 121.7 |
| PSS settled, MB | 106.4 | 107.2 |
| cgroup memory settled, MB | 36.3 | 37.0 |

correct after restore: 15/15

### restore-big-background-cold (n=9)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 680.3 | 724.0 |
| restore returned, CPU ms | 843.8 | 884.5 |
| ready (/readyz 200), wall ms | 846.9 | 886.4 |
| ready, CPU ms (all) | 1,116.9 | 1,172.6 |
| ready, CPU ms (runsc CLIs) | 53.9 | 64.8 |
| CPU in the 3 s after ready, ms | 81.3 | 86.4 |
| first exec (cat + psql), ms | 93.0 | 104.3 |
| app memory loaded at restore return (shmem), MB | 429.8 | 429.8 |
| PSS at ready, MB | 560.6 | 574.3 |
| PSS settled, MB | 563.7 | 565.0 |
| cgroup memory settled, MB | 1,521.2 | 1,523.7 |

correct after restore: 9/9

### restore-big-background (n=9)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 322.2 | 413.6 |
| restore returned, CPU ms | 640.1 | 793.0 |
| ready (/readyz 200), wall ms | 525.7 | 544.4 |
| ready, CPU ms (all) | 974.4 | 1,061.4 |
| ready, CPU ms (runsc CLIs) | 22.8 | 27.6 |
| CPU in the 3 s after ready, ms | 100.8 | 106.2 |
| first exec (cat + psql), ms | 67.0 | 73.4 |
| app memory loaded at restore return (shmem), MB | 429.7 | 429.9 |
| PSS at ready, MB | 563.1 | 573.4 |
| PSS settled, MB | 564.6 | 565.3 |
| cgroup memory settled, MB | 1,051.6 | 1,318.7 |

correct after restore: 9/9

### restore-big-cold (n=9)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 687.0 | 723.2 |
| restore returned, CPU ms | 864.7 | 883.4 |
| ready (/readyz 200), wall ms | 851.7 | 902.3 |
| ready, CPU ms (all) | 1,154.9 | 1,193.4 |
| ready, CPU ms (runsc CLIs) | 55.8 | 60.0 |
| CPU in the 3 s after ready, ms | 86.8 | 91.4 |
| first exec (cat + psql), ms | 94.5 | 108.8 |
| app memory loaded at restore return (shmem), MB | 429.8 | 429.8 |
| PSS at ready, MB | 561.2 | 564.3 |
| PSS settled, MB | 563.9 | 565.8 |
| cgroup memory settled, MB | 1,521.0 | 1,524.4 |

correct after restore: 9/9

### restore-big (n=9)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 496.2 | 532.7 |
| restore returned, CPU ms | 804.3 | 861.5 |
| ready (/readyz 200), wall ms | 603.5 | 635.7 |
| ready, CPU ms (all) | 1,008.5 | 1,086.8 |
| ready, CPU ms (runsc CLIs) | 24.7 | 27.7 |
| CPU in the 3 s after ready, ms | 94.8 | 132.2 |
| first exec (cat + psql), ms | 69.1 | 78.5 |
| app memory loaded at restore return (shmem), MB | 429.8 | 429.9 |
| PSS at ready, MB | 562.0 | 574.5 |
| PSS settled, MB | 563.8 | 565.0 |
| cgroup memory settled, MB | 1,317.7 | 1,386.0 |

correct after restore: 9/9

### restore-direct (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 159.2 | 166.7 |
| restore returned, CPU ms | 225.7 | 242.8 |
| ready (/readyz 200), wall ms | 221.8 | 240.4 |
| ready, CPU ms (all) | 364.5 | 383.2 |
| ready, CPU ms (runsc CLIs) | 25.5 | 33.7 |
| CPU in the 3 s after ready, ms | 49.4 | 56.9 |
| first exec (cat + psql), ms | 67.0 | 76.1 |
| app memory loaded at restore return (shmem), MB | 13.2 | 13.3 |
| PSS at ready, MB | 118.1 | 122.0 |
| PSS settled, MB | 106.3 | 107.0 |
| cgroup memory settled, MB | 62.5 | 63.8 |

correct after restore: 15/15

### restore-flate (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 197.0 | 206.4 |
| restore returned, CPU ms | 284.0 | 305.2 |
| ready (/readyz 200), wall ms | 267.0 | 283.9 |
| ready, CPU ms (all) | 427.4 | 449.3 |
| ready, CPU ms (runsc CLIs) | 25.6 | 35.8 |
| CPU in the 3 s after ready, ms | 50.0 | 60.1 |
| first exec (cat + psql), ms | 66.8 | 70.8 |
| app memory loaded at restore return (shmem), MB | 13.2 | 13.3 |
| PSS at ready, MB | 151.6 | 160.0 |
| PSS settled, MB | 142.2 | 146.2 |
| cgroup memory settled, MB | 73.6 | 78.7 |

correct after restore: 15/15

### restore-plain-cold (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 377.9 | 399.6 |
| restore returned, CPU ms | 312.0 | 328.2 |
| ready (/readyz 200), wall ms | 534.5 | 557.7 |
| ready, CPU ms (all) | 542.1 | 561.6 |
| ready, CPU ms (runsc CLIs) | 61.4 | 70.6 |
| CPU in the 3 s after ready, ms | 85.6 | 92.8 |
| first exec (cat + psql), ms | 107.4 | 122.4 |
| app memory loaded at restore return (shmem), MB | 13.2 | 13.2 |
| PSS at ready, MB | 117.5 | 122.9 |
| PSS settled, MB | 108.6 | 109.9 |
| cgroup memory settled, MB | 196.9 | 198.8 |

correct after restore: 15/15

### restore-plain (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 154.5 | 167.5 |
| restore returned, CPU ms | 220.7 | 247.6 |
| ready (/readyz 200), wall ms | 222.4 | 239.0 |
| ready, CPU ms (all) | 361.5 | 394.0 |
| ready, CPU ms (runsc CLIs) | 24.6 | 30.7 |
| CPU in the 3 s after ready, ms | 49.9 | 56.3 |
| first exec (cat + psql), ms | 63.1 | 69.5 |
| app memory loaded at restore return (shmem), MB | 13.2 | 13.3 |
| PSS at ready, MB | 117.5 | 122.5 |
| PSS settled, MB | 106.7 | 107.1 |
| cgroup memory settled, MB | 62.2 | 64.9 |

correct after restore: 15/15

### restore-zero (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| restore returned, wall ms | 150.7 | 168.6 |
| restore returned, CPU ms | 220.2 | 238.6 |
| ready (/readyz 200), wall ms | 222.5 | 234.6 |
| ready, CPU ms (all) | 367.8 | 390.7 |
| ready, CPU ms (runsc CLIs) | 25.5 | 31.0 |
| CPU in the 3 s after ready, ms | 75.6 | 108.0 |
| first exec (cat + psql), ms | 65.6 | 71.6 |
| app memory loaded at restore return (shmem), MB | 12.9 | 13.0 |
| PSS at ready, MB | 117.7 | 122.2 |
| PSS settled, MB | 108.3 | 109.6 |
| cgroup memory settled, MB | 49.3 | 50.8 |

correct after restore: 15/15

### warm-pool claim (subcontainer) (n=15)

| metric | p50 | p95 |
|---|---:|---:|
| warm sandbox start (outside the claim), wall ms | 104.1 | 128.7 |
| warm sandbox start, CPU ms | 138.3 | 159.3 |
| idle warm sandbox PSS, MB | 59.3 | 60.0 |
| idle warm sandbox cgroup memory, MB | 15.2 | 15.6 |
| claim: create+start subcontainer, wall ms | 57.1 | 73.6 |
| claim to ready (/readyz 200), wall ms | 394.6 | 439.4 |
| claim to ready, CPU ms (all) | 503.1 | 549.4 |
| PSS after claim, MB | 139.5 | 153.9 |

exec + workspace write ok: 15/15
