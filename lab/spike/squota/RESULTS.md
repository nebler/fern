# Spike: a disk limit per environment with btrfs simple quotas

**Question:** nothing limits what one environment writes, so one thread can
fill the pool and then no thread can snapshot. btrfs simple quotas
(`btrfs quota enable --simple`, Linux 6.7+) charge each extent to the
subvolume that allocated it, with no back-reference walks. With one group
(qgroup `1/N`) per environment holding its workspace and snapshots and a
limit on it: does the count say what the environment wrote and still holds,
does the limit stop a write, and what does it cost a snapshot, a restore and
a delete?

**Answer: yes, it works, and it costs no more than the noise here.** The
group counts exactly what the environment's own subvolumes allocated and
still hold; a write past the limit fails with `EDQUOT` ("Disk quota
exceeded"); snapshots and restores still work at the limit. Simple quotas
with every subvolume in a group cost a snapshot, a restore and a delete
somewhere between nothing and a quarter more on this machine, under a
spread of the unquoted runs that is larger than that, and nothing at all
with 20,000 files and 50 snapshots; classic qgroups take over 2 s per
snapshot there. Two defects of the kernel here (6.10) are below; one is fixed
upstream (6.17), the other is avoided by putting subvolumes in after
they are made.

Run 2026-11-13 with `lab/spike/squota/run.sh` (all sections; the timings
with the defaults, `ROUNDS=8 LIGHT=100 HEAVY=5 BIG=30`, 90 minutes), in a privileged
`golang:1.27` container on the Mac's Docker (Docker Desktop's VM, arm64,
12 CPUs, 16 GB) with a loop-mounted 8 GB btrfs image, as `lab/test-store.sh`
does. Not the lab's node and not the server.

| | |
|---|---|
| Container | Debian GNU/Linux 13 (trixie), Go 1.27.4 |
| Kernel | 6.10.14-linuxkit, aarch64 |
| btrfs-progs | v6.14 |
| `btrfs quota enable --simple` | works; `/sys/fs/btrfs/<uuid>/qgroups/mode` says `squota` |
| A group in sysfs | `qgroups/1_N/` has `exclusive`, `referenced`, `max_exclusive`, `max_referenced`, `limit_flags`: the daemon can read a group's use and limit without running a program |

**The server, unverified:** Ubuntu 24.04's packages (packages.ubuntu.com,
2026-11-13) are btrfs-progs **6.6.3**, which has no `--simple` (it came in
6.7), and the kernel `linux-image-generic` **6.8.0-149**, which has simple
quotas (merged in 6.7). The HWE kernels 6.11, 6.14, 6.17 and 7.0 are in
noble-updates. The server's own kernel was not looked at. The daemon can
turn simple quotas on and make groups with ioctls itself
(`BTRFS_IOC_QUOTA_CTL`, `BTRFS_IOC_QGROUP_CREATE`, `_ASSIGN`, `_LIMIT`), so
it needs no newer btrfs-progs; that was not tried.

## The count (section `accounting`, then `groups`)

Every step below is followed by a sync, and by waiting for btrfs's cleaner
when something was deleted. MiB as `btrfs qgroup show --raw` gives them.

One environment, group 1/1, each subvolume put in after it is made:

| Step | live | the snapshot | 1/1 |
|---|---:|---:|---:|
| live made, empty | 0.02 | | 0.02 |
| wrote 100 files of 1 MiB | 100.06 | | 100.08 |
| snapshot s1 | 100.06 | 0.02 | 100.08 |
| wrote 50 of the files again | 150.09 | 0.02 | 150.11 |
| s1 deleted | 100.06 | (stale, 0) | 100.06 |

The snapshot costs 16 KiB. The 50 MiB it holds stay charged to the
subvolume that wrote them, `live`, until the snapshot goes. So the group is
"what this environment wrote and still holds", snapshots included.

A golden in the builder's group (1/2), a thread started from it in 1/3:

| Step | 1/2 (builder) | 1/3 (thread) |
|---|---:|---:|
| builder wrote 200 MiB, snapshot golden | 200.16 | |
| thread's live is a writable snapshot of golden | 200.16 | 0.02 |
| thread wrote 10 MiB of its own | 200.16 | 10.09 |
| thread wrote 20 of the golden's files again | 200.16 | 30.11 |
| golden and the builder's workspace deleted | 180.03 | 30.11 |

A thread pays nothing for the golden it starts from, only for what it
writes. The golden's blocks stay charged to the builder's group for as long
as any thread holds them, even after the golden and its builder's workspace
are deleted: the deleted subvolume's group stays as a "squota space holder"
(`clear-stale` leaves it), with 180 MiB in it here.

A reflink copy is charged to whoever wrote the original: 30 MiB written in a
plain folder of the pool and `cp --reflink` into the thread's workspace
left the thread at 30.12 MiB (from 30.11), and the pool's top level (0/5)
keeps the 30 MiB after the plain folder is removed. An adopted worktree
therefore costs the thread nothing for its checkout.

A restore as `store.Restore` does it (snapshot s2, write 30 MiB, writable
snapshot of s2 as `live.new`, snapshot `pre-restore`, exchange, delete the
old workspace): 1/1 stays at 130.16 MiB, the old workspace's group becoming
a space holder; deleting `pre-restore` brings it to 100.08, and 10 MiB
written in the restored workspace to 110.11. A restore costs the group
nothing; what the old workspace wrote is charged until the last snapshot
holding it goes.

A fork in a group of its own (1/4) wrote 5 MiB: 1/4 says 5.05. Moved into
the thread's group with `qgroup remove` and `qgroup assign`, the 5.05 MiB
move with it (1/4 to 0, 1/1 up by 5.05).

**A subvolume can join a group after it is made, and brings what it holds:**
a subvolume that wrote 7 MiB and then joined 1/3 put 7.02 MiB in it. So
existing environments can be brought in, and a crash between "subvolume
made" and "put in its group" can be finished later.

**Enabling on a pool with data:** 29 ms on a pool holding 200 MiB.
Nothing that exists is counted: a workspace with 100 MiB and its snapshot
show 0. Rewriting 50 of the old files counts 50 MiB; deleting old files and
the old snapshot takes nothing below zero. A limit then counts only what
was written since. Turning quotas off and on again forgets every group,
limit and count.

### Two defects of this kernel

| What | Measured | Upstream |
|---|---|---|
| A group whose member subvolume was deleted can't be destroyed: `EBUSY` | `exclusive 0, exclusive_compressed 7356416` in the quota tree: the delete takes the bytes off `exclusive` and not off `exclusive_compressed` | "btrfs: fix squota compressed stats leak" (de134cb5), merged for 6.17-rc6 (Sept. 2025) and queued for 6.12-stable; not known whether Ubuntu's 6.8 has it |
| A group that never had a member, or whose member left before it was deleted, can be destroyed | `destroy 1/3`: ok | |
| Snapshots made with `-i 1/N` (inherit at creation) leave 16 KiB each in the group after they are deleted | 3 snapshots: 49152 bytes left in 1/1 after everything was deleted; 10 snapshots: 160 KiB | not found upstream; to check on the server's kernel |
| Snapshots put in with `qgroup assign` after they are made leave nothing | 1/2 back to 0 after all deleted | |

A subvolume made inside a subvolume that is in a group joins that group by
itself under simple quotas (the "contained" way: 1/3 in section
`accounting`), and leaks the same 16 KiB per snapshot as `-i`.

## The limit (section `limit`)

A limit of 150 MiB on 1/1 holding 100 MiB, written to as the box's user
(uid 1000):

- A writer of 64 KiB blocks got `write(2)` **errno 122, EDQUOT, "Disk quota
  exceeded"** after 49.88 MiB: the group then held 149.94 MiB.
- At the limit, as the box's user: `dd`, `cp`, `>small`, `mkdir`, `tar -x`
  and `git init` all fail with "Disk quota exceeded"; appending a line to a
  file that exists still works (its last block has room). As root too: the
  limit is the group's, not the user's.
- `rm` of a 50 MiB file that no snapshot holds frees the room at once: a
  5 MiB write right after works.
- **The daemon can still work at the limit:** a snapshot and a restore's
  writable copy both succeed, and add 16 KiB.
- `rm` of a file that the snapshot taken at the limit holds frees nothing:
  the next write fails. Deleting the snapshot frees it.
- A limit below what the group already holds is accepted, and the next
  write fails.

So a thread at its limit can't free space by deleting what an earlier turn's
snapshot holds; only deleting snapshots (or raising the limit) does.

## The cost (sections `bench` and `big`)

Four ways, each round starting one later than the last so no way always
follows the same one (round 1: off, simple, group, classic; round 2:
simple, group, classic, off; …), 8 rounds:

- **off:** no quotas.
- **simple:** `quota enable --simple`, no groups.
- **group:** simple quotas, the store's pools inside a subvolume in a limited
  group (100 GiB), so that every subvolume the store makes joins it by
  itself: the unchanged store, with one group above each subvolume.
- **classic:** `quota enable` and a rescan.

The store's `BenchmarkStore`, every case, 100 operations a run (5 for the
cases that make 20,000 files). Each run gives a median per case; the table
is the median of the 8 runs, in ms, with the change against off, and the
spread of off's own 8 runs:

| ms, median of the runs | off | off, least to most | simple | simple, in a limited group | classic |
|---|---:|---:|---:|---:|---:|
| Adopt20000                   | 996.19 | 809.46 to 3069.58 | 906.82 (-9%) | 905.05 (-9%) | 855.05 (-14%) |
| BuildSource20000             | 16.94 | 14.25 to 26.85 | 16.97 (+0%) | 20.85 (+23%) | 17.96 (+6%) |
| CreateEmpty                  | 3.83 | 3.48 to 21.39 | 3.40 (-11%) | 3.86 (+1%) | 3.79 (-1%) |
| CreateSeed20000              | 1085.30 | 790.39 to 4329.14 | 843.12 (-22%) | 973.91 (-10%) | 783.65 (-28%) |
| Delete10Snapshots            | 37.70 | 30.85 to 54.29 | 40.54 (+8%) | 46.34 (+23%) | 38.53 (+2%) |
| DeleteSnapshot               | 0.13 | 0.09 to 0.75 | 0.14 (+5%) | 0.17 (+26%) | 0.15 (+9%) |
| FindByPath                   | 0.03 | 0.01 to 0.11 | 0.01 (-45%) | 0.02 (-34%) | 0.01 (-53%) |
| FindByPath100                | 0.82 | 0.52 to 2.91 | 0.66 (-20%) | 0.69 (-16%) | 0.57 (-31%) |
| Get                          | 0.02 | 0.01 to 0.07 | 0.01 (-49%) | 0.01 (-38%) | 0.01 (-59%) |
| List                         | 0.03 | 0.01 to 0.15 | 0.01 (-53%) | 0.01 (-46%) | 0.01 (-45%) |
| List100                      | 0.81 | 0.60 to 3.15 | 0.64 (-21%) | 0.70 (-13%) | 0.55 (-32%) |
| Reconcile                    | 0.06 | 0.03 to 0.25 | 0.04 (-32%) | 0.05 (-21%) | 0.04 (-35%) |
| Restore                      | 9.04 | 6.87 to 28.00 | 7.02 (-22%) | 9.94 (+10%) | 12.23 (+35%) |
| SnapshotClean                | 3.28 | 2.46 to 16.02 | 3.02 (-8%) | 3.69 (+12%) | 4.62 (+41%) |
| SnapshotDirty8MiB            | 23.94 | 21.13 to 126.09 | 22.05 (-8%) | 25.81 (+8%) | 23.93 (-0%) |
| Snapshots50                  | 0.27 | 0.24 to 1.19 | 0.24 (-10%) | 0.30 (+11%) | 0.24 (-13%) |

A larger case the benchmark lacks (`BenchmarkSpikeBig`, written by the
script into a copy of the store's tests): an environment of 20,000 files of
4 KiB with 50 snapshots, 200 files written again between each. Each
operation times one more snapshot, a snapshot's delete, the wait for btrfs's
cleaner to drop it (`delete-cleaned`) and a restore to 25 turns back. 30
operations a run, 8 runs a way; here **group** is the design the brief
proposes: no containing subvolume, each subvolume put into its group with
`BTRFS_IOC_QGROUP_ASSIGN` right after it is made, timed with the operation:

| ms, median of the runs | off | off, least to most | simple | simple, in a limited group | classic |
|---|---:|---:|---:|---:|---:|
| snapshot                     | 17.96 | 10.52 to 185.79 | 15.54 (-13%) | 10.26 (-43%) | 2226.27 (+12299%) |
| delete                       | 0.25 | 0.19 to 0.43 | 0.30 (+19%) | 0.17 (-31%) | 0.52 (+105%) |
| delete-cleaned               | 10.05 | 5.78 to 53.64 | 8.56 (-15%) | 5.15 (-49%) | 2350.57 (+23288%) |
| restore                      | 28.56 | 14.54 to 183.53 | 19.86 (-30%) | 14.49 (-49%) | 2183.40 (+7546%) |

**Classic qgroups are out:** with 50 snapshots sharing 20,000 files, each
snapshot, restore and cleaned delete takes over 2 seconds, as every commit
walks the back-references of what changed. Simple quotas, with or without
the groups, are not slower than off here.

Other worktrees' `go test` runs used the same Docker VM during part of these
rounds (seen at 15:20, rounds 4 to 8 of this case); the rotation spreads that
over the four ways but not evenly.

**How noisy:** very. Cases that touch no btrfs code at all (`Get`, `List`,
`FindByPath`) come out 30 to 60% faster with quotas on than off, which no
quota can cause: off's runs spread over a factor of 3 to 6, and its medians
carry that. Only differences of tens of percent that line up across cases
mean something here. Read that way: simple quotas alone cost nothing
measurable; putting every subvolume in a group adds roughly 10 to 25% to
the cases that make or delete a subvolume (a snapshot, a restore, a
delete); classic qgroups add 35 to 40% to a clean snapshot and a restore,
and in the larger case over 2 s to each of them. This is a VM on a Mac with a loop file on its virtual disk,
not the server's disk: the numbers to decide on are the server's.

## The box's alternative (no experiment)

- **gVisor:** runsc's only size option is `--overlay2={root|all}:{medium}[,size=…]`,
  which caps the tmpfs (or file) *upper layer of an overlay* that runsc puts
  over a mount (`runsc/config/config.go`: "Passed as is to tmpfs mount, as
  `size={size}`"; gVisor's filesystem guide says the same). With it, the
  box's writes land in that layer and never reach the workspace on the host,
  so Fern's snapshots would not see them: it is not a limit on a
  bind-mounted host folder. There is no other disk flag (`dcache` and
  `fdlimit` are a cache's and descriptors' counts).
- **The OCI runtime spec** has no field that limits disk space for a mount or
  the root filesystem: `linux.resources.blockIO` holds weights and
  bandwidth and IOPS throttles, not bytes.

So the limit has to be the filesystem's: btrfs's groups, or a filesystem per
environment (which would lose the snapshots and reflinks Fern is built on).
