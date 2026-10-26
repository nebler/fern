# Box memory, 2026-10-25

Measured from `444bcd95`, ARM64 Docker Desktop lab, gVisor 20261012.
No VPS access, model requests, baseline edits or full suite. Every deploy and
measurement shared one `lab_lock`; the profiling/A/B batch took about four
minutes. The runtime experiments were removed before releasing the lock.

## What the gap contains

The 47 MiB box metric and the 90 MiB VPS scale metric describe different
states. `box` samples after repeated sleep/wake cycles; `scale` samples just
after `initdb`. The lab's `free` metric also differs from modern procps
measurements.

The lab's BusyBox 1.37 computes used memory as
`MemTotal - MemFree - Buffers - Cached - SReclaimable`. This subtracts shared
application memory backed by gVisor's memfd along with `Cached`. Modern procps
computes used memory as `MemTotal - MemAvailable`.
Sources: [BusyBox](https://raw.githubusercontent.com/mirror/busybox/master/procps/free.c),
[procps](https://gitlab.com/procps-ng/procps/-/raw/master/man/free.1),
[gVisor accounting](https://gvisor.dev/docs/architecture_guide/resources/).

Five boxes, direct same-lock profile, MiB per box:

| Charge | Fresh database | After sleep/wake |
|---|---:|---:|
| Sentry native anonymous memory | 18.1 | 11.6 |
| Two gofers, native anonymous memory | 11.6 | 11.7 |
| Systrap subprocess anonymous memory | 0.1 | 0.1 |
| Application/shared memory | 25.6 | 24.4 |
| **Pod anon + shmem** | **55.3** | **47.8** |
| Pod kernel memory | 7.9 | 4.4 |
| Two containerd shims outside the pod, anonymous memory | 8.9 | 9.0 |
| Two `runsc wait` helpers outside the pod, anonymous memory | 6.5 | 6.5 |
| Kernel increment outside pods in the node's init cgroup | 1.2 | 1.3 |
| k3s anonymous + kernel increment, amortized | 2.9 | 2.9 |
| **Observed node used increment, MemAvailable definition** | **82.0** | **72.0** |

Rows overlap at the bold pod subtotal. Small differences between the component
sum and the node estimate reflect sampling and reclaimability, especially slab.
The corresponding BusyBox node increments were only 56.3 and 47.5 MiB per box.

The fresh Sentry retains about 6.5 MiB more native anonymous memory than the
warm Sentry; application shmem changes by only about 1.2 MiB. Warm pod kernel
memory is approximately 1.95 MiB page tables, 1.10 MiB kernel stacks, 1.08 MiB
slab and 0.26 MiB other kernel allocations.

An empty box had 20.8 MiB runtime anon, 1.8 MiB application shmem and 2.0 MiB
pod kernel memory. Adding the sample database increases warm application shmem
by about 22.6 MiB. Guest `/proc` RSS is unsuitable for adding these costs:
gVisor approximates it and shared Postgres mappings appear in multiple processes.
The host measurements use cgroup charges and PSS anonymous fields.

The box starts `fern-init`, pause, the Postgres service's shells and Postgres's
workers. It starts neither Node nor Claude for this sample. T3 runs in a separate
pod. Image pages are shared; the image's unpacked size is not a per-box charge.
The fresh database also creates considerable reclaimable file cache, which must
not be added to the anon/shmem numbers as another persistent allocation.

This is a measured lab breakdown, not an exact VPS process attribution. In
particular, subtracting a **warm** 47 MiB pod charge from a **fresh** 90 MiB node
increment does not establish that all 43 MiB lives outside the pod.

## Rejected runtime changes

Exploration used `SCALE="1 5" WAKES=5`; the build and all perf invocations stayed
inside one enclosing lock. A is one immediately adjacent unchanged run;
B is the median of three runs; C is one exploratory run after B.

| Metric | A: defaults | B: shim grouping | C: grouping + helper GOGC=20 |
|---|---:|---:|---:|
| Warm pod anon + shmem, MiB | 46.8 | 47.3 | 47.6 |
| Fresh five-box pod anon + shmem, MiB each | 55.3 | 55.8 | 55.1 |
| Five-box node increment, MemAvailable, MiB each | 88.4 | 70.5 | 83.1 |
| Legacy five-box node increment, MiB each | 63.2 | 44.6 | 56.8 |
| Warm wake p50, ms | 762.4 | 758.5 | 712.8 |
| Exec p50 with five boxes, ms | 84 | 80 | 78 |

All gates passed. Grouping changed five boxes from ten to five shims. The
direct matched-state profile shows only about **4.0 MiB per box** of helper
anonymous memory saved, with pod charges unchanged. That is below the 8 MiB
memory floor and the lab's noise margins. The much larger apparent node-median
saving is inconsistent with the attributable process saving; it is not accepted
as an 18 MiB win. Grouping remains disabled.

Setting `GOGC=20` in the shim launcher (inherited by runtime helpers, not the
Sentry or application) gave no useful pod-memory reduction. Its one-run node
result did not improve on grouping; it was removed without spending another
three-run confirmation on it.

KVM was unavailable (`/dev/kvm` absent). Application huge-page tuning had no
new angle: host shmem huge pages were already disabled. The previously rejected
Postgres buffer/connection tuning and root-overlay change were not repeated.

## Kept measurement change

`check.sh scale` retains the existing free-based metric and adds:

- `node_used_per_box_mib`: `(MemTotal - MemAvailable)` delta / awake boxes,
  with a consistent meaning on both hosts;
- `box_memory_mib`: all measured boxes' current cgroup anon + shmem / box count;
- `pod_kernel_mib`: current kernel charges / box count.

These are reporting corrections, not claimed memory savings. The cgroup reads
are batched into one node call and fail the section if the charges cannot be
read. Existing baseline keys and files are unchanged.

Reproduce the timing gate and matched-state pod measurements:

```sh
bash -c 'source lab/lib.sh; lab_lock; lab/build.sh && SCALE="1 5" WAKES=5 REPEAT=3 lab/perf.sh box scale'
```

Validation: `lab/dev.sh box scale limits` with `SCALE="1 5" WAKES=2 JOBS=1`
passed (race tests, build and real-node checks), as did `go vet ./...`,
`golangci-lint run`, `gofmt -d .`, shell syntax and diff whitespace checks.

The largest remaining idle costs are Postgres/application pages (~24 MiB)
and the Sentry plus gofers (~23 MiB). Eliminating runtime processes or returning
the Sentry's startup heap is a more substantial next angle than tuning
`fern-init`. Claude's active-session memory is an additional, workload-dependent
cost; it was not started in this experiment.

## Second pass from `fc7dd98b`

Branch `t3/reduce-box-memory-1`, the same ARM64 lab and pinned runtime. Every
build and measurement shared one enclosing `lab_lock`; no VPS, node restart,
baseline edit, merge or push. The three-run starting gate (`SCALE="1 5"`,
`WAKES=5`, `REPEAT=3`, `box scale`) passed:

| Metric | Starting median |
|---|---:|
| Warm pod anon + shmem | 47.4 MiB |
| Fresh five-box pod anon + shmem, each | 55.8 MiB |
| Five-box node increment, MemAvailable, each | 90.4 MiB |
| Warm wake p50 | 775.9 ms |
| Five-box exec p50 | 69 ms |

### Accounting cross-check

A separate five-box control profile after one sleep/wake gave this breakdown.
These are host cgroup charges and native PSS anonymous pages, not a sum of
Postgres's guest RSS mappings:

| Warm charge, MiB per box | Control |
|---|---:|
| Application/shared memory | 24.4 |
| Sentry, including small platform allocations | 11.9 |
| Two gofers | 11.8 |
| Two containerd shims | 9.2 |
| Two `runsc wait` processes | 6.4 |
| Pod kernel | 4.5 |
| **Directly attributed subtotal** | **68.3** |
| Other node charges, from the lab cgroup's increment | 7.0 |
| **Lab cgroup anon + shmem + kernel increment** | **75.2** |
| Global MemTotal - MemAvailable increment | 78.4 |

The global `/proc/meminfo` in this Docker lab describes the Docker VM, including
other containers. One ptrace scale sample claimed 1,165 MiB for its first box
while that pod's anon + shmem was only 56.8 MiB. Even the lab cgroup includes
k3s's changing Go heap: a one-box Postgres control overlapped a collection and
had a negative whole-node increment. Neither is evidence of a box saving.
Use matched pod/process charges to attribute a change; node deltas are a
cross-check, especially with five boxes to amortize background variation.

File cache was recorded separately as `memory.stat`'s `file - shmem` at both
pod and lab-cgroup levels. On the control's sleep/wake, the pod's file-cache
charge fell 77.8 → 2.9 MiB per box, but the whole lab's file-cache increment
stayed 37.9 → 37.4 MiB per box. Destroying a cgroup can move its surviving cache
charges to an ancestor. That apparent pod saving must not be counted as freed
RAM. The anon + shmem and native-process comparisons below avoid it.

### Runtime experiments

Each row had one `perf.sh box scale` run, five wakes, plus an independent
matched-state profile of five boxes. The memory columns are from that profile;
timings are the preceding standard perf run. These are screening runs, not
three-run confirmations of a shipped change.

| Runtime | Fresh pod MiB | Warm pod MiB | Warm kernel MiB | Wake p50 ms | Exec p50 ms |
|---|---:|---:|---:|---:|---:|
| Defaults, systrap | 55.5 | 48.1 | 4.5 | 687 | 66 |
| `--platform=ptrace` | 55.4 | 47.5 | 5.9 | 1,979 | 90 |
| `--file-access=shared --overlay2=none --dcache=0` | 52.5 | 47.8 | 4.4 | 707 | 71 |
| `--directfs=false` | 58.9 | 49.7 | 5.1 | 794 | 72 |

All are rejected. Ptrace grew kernel memory and was substantially slower.
The filesystem bundle saved about 3 MiB fresh and only 0.3 MiB warm; its
standard node metric's apparent 18.7 MiB saving was not attributable to the
runtime. Disabling directfs grew gofer anonymous memory 11.8 → 13.3 MiB per
box. Every profile still had two gofers, two shims and two wait processes.
Some timing gates failed even on unchanged controls; no noisy node-wide
improvement was substituted for the direct profiles.

`GOMEMLIMIT=4MiB` in the runsc launcher, inherited by helpers and gofers,
failed the box check after forced-pod recovery: the seeded-row check expected
1 and got no result. Its timings were discarded. It is not a kept tuning.

The box has one application container, no init containers and no service
sidecars. Postgres is a child of fern-init. The other CRI container is pause,
which reaps orphans under the shared PID namespace. The shim starts a persistent
[`runsc wait` for each container](https://github.com/google/gvisor/blob/release-20261012.0/pkg/shim/v1/proc/init.go#L298)
to publish its exit status. Removing service sidecars cannot eliminate these
pairs; there are none to remove. Shim grouping was not repeated.

The pinned runtime already enables directfs and disables host UDS access;
non-root mounts already use shared file access. [Directfs keeps the gofer
process](https://gvisor.dev/docs/user_guide/filesystem/). There is no Sentry
GOGC/GOMEMLIMIT flag in this release, and its
[launch environment is cleared](https://github.com/google/gvisor/blob/release-20261012.0/runsc/sandbox/sandbox.go#L917).
The boot-only `total-host-memory` argument is not a supported heap/reclaim
control. KVM remains unavailable in this lab. No containerd configuration is
kept from these trials, so these results require no lab restart.

### Postgres screen

Four sequential configurations, each with three warm restarts, 20 execs, and
three 15-second `pgbench -n -c 4` writes against scale 5. Each started from a
fresh sample database. Durability, autovacuum, preload libraries and JIT were
left unchanged. Memory excludes reclaimable file cache; loaded means after
the three write runs.

| shared_buffers / max_connections | Warm pod MiB | Loaded pod MiB | Wake p50 ms | Exec p50 ms | Write latency median ms | TPS median |
|---|---:|---:|---:|---:|---:|---:|
| 128 MiB / 100 | 47.0 | 128.5 | 839 | 70.5 | 12.214 | 327.5 |
| 32 MiB / 20 | 39.7 | 80.9 | 740 | 69.5 | 23.955 | 167.0 |
| 64 MiB / 20 | 41.9 | 114.6 | 2,476 | 251.5 | 10.765 | 371.6 |
| 128 MiB / 20 | 44.9 | 124.2 | 747 | 69.0 | 14.899 | 268.5 |

The 32 MiB variant saved 7.3 MiB warm, below the old guest-RSS estimates.
Its individual write runs were 406, 167 and 84 TPS, versus the control's 254,
327 and 358. This does not support keeping it. The 64 MiB run also overlapped
large wake/exec variation, so these sequential screen medians alone do not
establish a performance benefit or regression for every setting.

The control reported `huge_pages_status=on` despite disabled host shmem huge
pages. That status means PostgreSQL's mmap request succeeded. The pinned
[gVisor mmap implementation](https://github.com/google/gvisor/blob/release-20261012.0/pkg/sentry/syscalls/linux/sys_mmap.go)
does not select a special allocator for `MAP_HUGETLB`, so the status alone does
not establish that physical huge pages are in use. An explicit on/off control
gave the following three-run write medians:

| Configuration | Warm pod MiB | Application shmem MiB | Loaded pod MiB | Wake p50 ms | Exec p50 ms | Write latency ms | TPS |
|---|---:|---:|---:|---:|---:|---:|---:|
| 128 MiB / 100, huge pages `try` | 47.2 | 24.39 | 131.0 | 745 | 68.5 | 11.746 | 340.5 |
| 128 MiB / 100, huge pages `off` | 47.0 | 24.36 | 131.8 | 748 | 67.0 | 10.914 | 366.5 |
| 32 MiB / 20, huge pages `off` | 40.0 | 16.93 | 80.8 | 695 | 85.5 | 13.164 | 303.9 |

Disabling huge pages changed shared memory by only 0.03 MiB and did not reduce
loaded memory. It is rejected. The second small-buffer screen again declined
across its write runs (407 → 304 → 214 TPS), and its median latency was 12%
above the adjacent 128 MiB control. This reinforces keeping the larger cache
while testing other resident allocations.

### Resident bookkeeping, keeping the 128 MiB cache

The selected sample settings are `max_connections=20`, `wal_buffers=512kB`
and `max_logical_replication_workers=0`. They remove unused connection/WAL
bookkeeping and the logical replication launcher. Shared buffers stay at
128 MiB; autovacuum, JIT, fsync and other durability settings are unchanged.
These are the sample service's arguments, including when it starts an existing
database. The start script is already a golden-key input.

A stronger control reused one scale-5 database and alternated the old and new
settings twice. Each phase had three restarts, 20 execs, a discarded 15-second
write warmup, and three measured 15-second writes. All four phases recorded
zero `pg_stat_wal.wal_buffers_full` events.

| Phase | Warm pod MiB | Loaded pod MiB | Wake median ms | Exec median ms | Write latency median ms | TPS median |
|---|---:|---:|---:|---:|---:|---:|
| Control 1 | 47.0 | 130.8 | 743 | 66.0 | 12.624 | 316.8 |
| Candidate 1 | 40.2 | 120.5 | 701 | 68.0 | 14.189 | 281.9 |
| Control 2 | 47.2 | 127.6 | 739 | 67.5 | 13.945 | 286.8 |
| Candidate 2 | 39.7 | 122.9 | 981 | 65.0 | 13.492 | 296.5 |

The repeated warm reduction was 6.8 and 7.5 MiB. Across both phases, wake
medians were 741.1 → 737.8 ms and exec medians 66.5 → 66.0 ms. Pooled write
latency was 13.285 → 13.841 ms (+4.2%); the individual control runs spanned
8.954–23.725 ms and candidate runs 11.898–16.180 ms. This lab variability
cannot resolve a write difference that small; the full snapshot-load check
below provides another measurement.

The final five-box profile used an unmodified `fc7dd98b` start script for the
control and the checked-in candidate for B, under one build/measurement lock.
Warm pod anon + shmem was **48.0 → 40.1 MiB**, and fresh pod memory was
55.4 → 47.4 MiB. The warm breakdown, MiB per box:

| Charge | Control | Candidate |
|---|---:|---:|
| Application/shared memory | 24.4 | 17.0 |
| Sentry and small platform allocations | 11.9 | 11.7 |
| Two gofers | 11.7 | 11.5 |
| Two shims | 8.7 | 8.7 |
| Two wait helpers | 6.5 | 6.4 |
| Pod kernel | 4.4 | 4.1 |
| **Directly attributed subtotal** | **67.5** | **59.4** |

The candidate's lab-cgroup anon + shmem + kernel increment was 67.4 MiB per
box, including about 8 MiB of other node charges; the global MemAvailable
increment was 66.8 MiB. These node estimates remain variable. The matched
control overlapped a k3s collection, illustrating again why the direct charges
are the evidence for the saving. Warm pod file cache was 2.8 → 2.5 MiB, and
the database cache size was preserved: the resident reduction is not a claimed
migration into uncounted file cache. The direct subtotal fell about 8.1 MiB;
the application/shared part accounts for 7.4 MiB of it.

### Go limits and explicit Sentry reclaim

A temporary `debug.SetMemoryLimit(4 << 20)` in fern-init's main applied the
budget only to fern-init, without restricting child commands. The control and
trial measured 47.2 → 48.8 MiB per warm pod, wake 769 → 899 ms, and exec
70 → 76 ms. There was no measured saving. The source change was removed.

A standard-library probe called the pinned Sentry's `Usage.Reduce` RPC with
`wait=true` and garbage collection enabled. Both trials used five boxes; each
100-exec batch ran 20 commands in each box:

| State, MiB per pod | Before RPC | After RPC | After 100 normal execs |
|---|---:|---:|---:|
| Fresh start followed by 100 execs | 57.20 | 56.50 | 57.49 |
| After sleep/wake | 47.91 | 47.95 | 50.41 |

The first RPCs cost median 17.94 ms and exec p50 changed 68 → 70 ms. The warm
RPCs cost median 2.58 ms; subsequent exec p50 was 67 ms. Shared application
memory did not fall. The small first reduction did not survive ordinary use,
and the warm trial saved nothing. No reclaim hook, profiling-mode security
change or runtime wrapper is retained.

### Validation and retained measurement

`lab/check.sh box scale limits pgload` passed on the unchanged control, with
`JOBS=1 SCALE="1 5" WAKES=5 METRICS=1`, build included in the lock (6 min
23 s). All 50 retained snapshots recovered with whole transactions and passed
`pg_amcheck`. Pgbench latency under snapshots was 15.501 ms; snapshot p50/p95
was 33.1/215.4 ms.

`pgload` now retains pgbench's output and reports `pgload.latency_ms`. It
previously threw the write-performance result away. This is a measurement
change, with no claimed memory saving and no baseline changes. It also
explicitly fails a section when the pgbench write process exits unsuccessfully:
`run_section`'s `||` failure collection disables Bash errexit in its body, so a
bare `wait` was insufficient to ensure that the load actually succeeded.

The candidate passed the same `box scale limits pgload` check in 6 min 2 s,
with all 50 snapshot restores whole and uncorrupted. Snapshot-load write
latency was 15.501 → 6.637 ms and snapshot p50/p95 33.1/215.4 → 18.2/35.4 ms.
Given the control variation above, these results support retaining the memory
change without claiming a write-speed improvement. Box memory was 47.5 →
40.1 MiB, warm wake p50 886 → 768 ms and five-box exec p50 66 → 68 ms.

The first final three-run timing gate reported warm memory 39.6 MiB, wake
p50/p95 739.9/838.5 ms and five-box exec p50 70 ms. It failed the legacy fresh
five-box wake-p50 and parallel-exec gates (3,935.5 ms and 123 ms). The adjacent
matched five-box control's fresh wake was 5,847 ms versus the candidate's
4,988 ms, so the old fresh-wake comparison also reflects lab drift. A final
five-run repeat was queued, but the shared build lock remained held by a remote
benchmark through the remaining three-hour time box. The repeat was canceled
while still waiting; it never built or measured anything. **The legacy perf
gate remains uncleared on those two scaling metrics.** The matched timings and
all requested correctness checks above are the evidence retained with this
local change. Baselines and tolerances were not changed.

Race tests, golangci-lint (0 issues), go vet, gofmt, shell syntax and whitespace
checks passed. No Go source changes remain.


## Claude process accounting, 2026-10-26

The hardware gate's two toy runs per target sampled six completed Haiku
turns. Host pod anon + shmem peaks were 174–217 MiB on the VPS and
172–204 MiB in the lab; matched service-only boxes used 38.4/37.8 MiB.
Median increments, 152/158 MiB, include Claude, its tools and runtime growth.

Inside gVisor, Claude-tree peak RSS/anonymous estimates ranged
483–765/208–292 MiB on the VPS, 480–517/205–243 MiB in the lab.
Other guest processes' median peak RSS/anonymous estimates were
157/50 MiB and 156/49 MiB respectively. Guest status lacks RssAnon here;
the sampler sums smaps Anonymous. These guest figures approximate residency
and can count shared mappings repeatedly, explaining why they exceed the
entire host pod's charge. Use the matched host increment for capacity;
exact process-level host attribution remains unavailable.

The guest sampler reads PID, parent PID, start time, CPU, RSS and anonymous
fields every two seconds. Ancestry identifies Claude's tree; start time
distinguishes reused PIDs; CPU accounting retains ancestry after reparenting.
CPU is a lower bound because children can start and exit between polls.
Box CPU comes from pod cgroups, including Sentry/gofers. Observer execs
add a small cost; guest sampling is disabled during restore to avoid waking
an environment for measurement. See [RESULTS.md](RESULTS.md) for operation,
platform, disk and lifecycle measurements.
