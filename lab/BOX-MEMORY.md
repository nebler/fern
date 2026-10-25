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
