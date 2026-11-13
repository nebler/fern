# A disk limit per environment

**Recommendation: build it with btrfs simple quotas, one group per
environment, each subvolume put into its group right after it is made; turn
it on only after the four measurements at the end pass on the server.** The
spike ([lab/spike/squota/RESULTS.md](../../lab/spike/squota/RESULTS.md),
2026-11-13, Mac's Docker, kernel 6.10) found that the group counts exactly
what an environment wrote and still holds, snapshots included; that a write
past the limit fails with "Disk quota exceeded" while the daemon can still
snapshot and restore; and that the cost was within the noise of that
machine (the store's benchmark: a clean snapshot 3.3 ms off, 3.7 ms with every
subvolume in a group, a restore 9.0 and 9.9 ms, with off's own runs spread
from 2.5 to 16 ms; with 20,000 files and 50 snapshots, a snapshot 18 ms off
and 10 ms with groups). Classic qgroups are out: in that larger case they
took 2.2 s for each snapshot, restore and delete. A limit at
the box is not possible: neither gVisor nor the OCI spec has a disk limit for
a bind-mounted host folder.

## What it means for a thread

- **What counts:** every block the environment's workspace allocated and
  that its workspace or one of its snapshots still holds. A snapshot itself
  costs 16 KiB. A golden costs the thread nothing: its blocks stay charged
  to the repository's builder, also after the golden is pruned, until the
  last thread holding them lets go. An adopted worktree's checkout is charged
  to whoever wrote it (T3), not the thread. A restore costs nothing.
- **At the limit,** a new file, `mkdir`, `git` and builds fail with EDQUOT;
  the thread's next snapshot and a revert still work. Deleting a file frees
  room only if no snapshot holds it; with 50 kept, what a thread built and
  deleted in the last turns stays charged. So the way out is a bigger limit
  or fewer snapshots, which the person decides, not Claude.
- **The pool guard stays:** the limits together may add up to more than the
  pool; a limit stops one thread, the guard (ErrPoolFull) stops all.

## Design, in build order

1. **Turn it on.** The daemon enables simple quotas at start when
   `--env-disk-limit` is set (by ioctl, `BTRFS_IOC_QUOTA_CTL`, so that it
   doesn't depend on the node's btrfs-progs having `--simple`), and leaves the pool alone
   otherwise. Existing data is never counted: a limit counts from then on.
   The kernel's quota tree holds the groups and limits: it is on the disk,
   so nothing else records them.
2. **A group per environment** (`internal/store`). The record gets
   `"group": N` (qgroup `1/N`). `create` picks N as one more than the
   largest group in `/sys/fs/btrfs/<uuid>/qgroups/` (never one that exists,
   as a deleted environment's group can outlive it) and writes it in the
   marker; then it makes the group and sets the limit, then the workspace,
   then the record as now. A fork and a builder are environments, so each
   gets its own group; the builder's gets no limit, since it pays for goldens
   that threads hold.
3. **Every subvolume joins its group right after it is made**
   (`BTRFS_IOC_QGROUP_ASSIGN`), not with inherit at creation: inherit
   leaves 16 KiB per deleted snapshot in the group on 6.10, assign leaves
   nothing, and a subvolume that joins late brings what it holds, so a crash
   in between loses nothing. All subvolumes come from `createSubvolume` and
   `snapshotSubvolume` (`ioctl.go`), called in seven places:
   `makeWorkspace` (new empty workspace), `adopt` (empty, or onto a golden),
   `forkWorkspace`, `BuildSource` (the builder's workspace), `snapshot`
   (turns, pre-restore, `golden-<key>`, `golden-source`) and `restore` (the
   copy, before the swap). Each joins the group of the environment it
   belongs to.
4. **Crashes** (`Reconcile`, `clearLeftovers`): a marker with a group and no
   record means "group made, environment not": delete what it left as now,
   then destroy the group. An environment's newest subvolumes (its workspace
   and newest snapshot, the only ones a crash can leave outside) join again
   at start; joining twice is harmless (EEXIST).
5. **Delete:** after its subvolumes, destroy the group. It refuses (EBUSY)
   while a deleted subvolume's blocks are still held elsewhere (a builder's
   golden), and on kernels without the upstream fix (6.17, queued for 6.12-stable:
   "fix squota compressed stats leak"), always once a member was deleted. Then leave it: `fern gc` and Reconcile destroy
   groups no record names once they can. A leftover group is 40 bytes and
   counts nothing.
6. **Bringing in existing environments:** at the first start with quotas,
   make each environment's group, write it to its record, and put its
   workspace and snapshots in. Their old data stays uncounted.
7. **Seeing it:** the group's use and limit are files in sysfs
   (`qgroups/1_N/exclusive`, `max_referenced`), read without a program.
   `fern df` adds a column per environment, "written, of limit".
   `fern doctor` lists an environment at 90% or more as a problem, and says
   when quotas are off or the kernel too old. `fern limit <env> <size>` sets
   one environment's limit (the person's call).
8. **Telling the thread:** at 90% and at the limit, a notice in Claude's
   hint (the same path as a crashing box's): "this thread has used X of its
   Y GiB; deleting files won't free what earlier turns' snapshots hold; ask
   the person to raise the limit". The person gets the same through
   `--notify-url`. The daemon's look each minute already exists; it reads
   one sysfs file per environment.

## Measure on the server before turning it on

Read on the server on 2026-11-13, after this brief was written: Ubuntu
26.04.1, kernel 7.0.0-38, btrfs-progs 6.17.1 with `--simple`. The kernel
is past the upstream fix (6.17), so the group-destroy fault above should not
occur there; item 1 below still checks it on a loop image.

1. **The kernel:** `uname -r`, and whether its quota destroy works after a
   member is deleted (`lab/spike/squota/run.sh groups` there, by hand, on a
   loop image, not the pool). If not, move to the HWE kernel (6.17 or 7.0 in
   noble-updates) or accept leftover groups.
2. **The cost where it counts:** `lab/ab.sh -s "box"` with quotas off (A) and
   on with groups (B), on a copy of the pool's setup: a snapshot (6–8 ms),
   a restore (60–80 ms) and a wake (~150 ms) must not move beyond the gate's
   margins; then `lab/perf.sh` with the baseline.
3. **Turning it on with 60 GB of data:** how long the enable takes (29 ms
   on 200 MiB here; it walks nothing), and that nothing stalls.
4. **The count against reality:** the scriptc thread's group after a day of
   turns, against `btrfs filesystem du` of its workspace and snapshots.
