# Local Go performance measurements

## Method

- Machine: Apple M4 Pro, 24 GiB RAM, macOS ARM64.
- Toolchain: `go1.27.0 darwin/arm64`; environment `CGO_ENABLED=1` (taskstore uses
  pure-Go modernc SQLite).
- Source: uncommitted working tree based on `2a333f8`, after schema 3/resource-spec
  10 and the harness-owned GitHub handoff.
- The original results used the package name `jsoncanon`, since renamed to
  `strictjson` without parser changes. The reproduction command uses its current
  path; these numbers are the original baseline, not a claimed cleanup speedup.
- One package benchmark process at a time (`-p=1`), one Go scheduler processor
  (`-cpu=1`), three samples, one-second target per benchmark.
- No Go race instrumentation, coverage instrumentation, concurrent test suite,
  or live Docker harness was running during measurement. Host background
  activity and filesystem caches are not controlled.

```sh
go test -p=1 -run '^$' -bench . -benchmem -benchtime=1s -count=3 -cpu=1 \
  ./internal/config ./internal/control ./internal/githubapp \
  ./internal/strictjson ./internal/observability ./internal/taskstore \
  ./internal/backgroundroute ./internal/taskartifact
```

The benchmark implementations are checked in next to their packages. Unit-test
execution duration is not used as operation latency. Setup is outside timing
unless the fixture description below explicitly says otherwise. Go allocation
counts exclude memory allocated by child Git processes and the OS.

## Results

Values below are the **median of three benchmark sample averages**, not
per-request p50/p95/p99 latency. Ranges are the smallest/largest sample average.
Allocation columns use the median reported sample. Bytes are bytes, not KiB.

| Benchmark | Median time/op | Sample range | B/op | Allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Config load | 66.52 µs | 63.85–68.16 µs | 57,506 | 578 |
| Task policy parse | 22.74 µs | 22.32–22.76 µs | 32,825 | 241 |
| Config validation | 2.813 µs | 2.755–2.919 µs | 1,524 | 18 |
| Cached device auth, 1 device | 111.0 ns | 110.3–118.5 ns | 128 | 2 |
| Cached device auth, 64 devices | 119.4 ns | 118.8–122.5 ns | 128 | 2 |
| Strict GitHub JSON decode | 2.542 µs | 2.501–2.563 µs | 1,504 | 63 |
| JSON check: run request | 743.9 ns | 723.6–747.1 ns | 912 | 28 |
| JSON check: 64 history entries | 50.104 µs | 49.323–50.255 µs | 29,920 | 1,817 |
| JSON check: late duplicate key | 49.839 µs | 49.535–50.794 µs | 30,000 | 1,823 |
| Registry snapshot: ready | 54.65 ns | 53.64–55.14 ns | 288 | 1 |
| Registry snapshot: blocked | 55.40 ns | 53.11–59.75 ns | 288 | 1 |
| Registry snapshot: parallel readers, one Go CPU | 53.37 ns | 53.11–57.16 ns | 288 | 1 |
| Fresh durable run admission | 1.791 ms | 1.660–1.821 ms | 15,533 | 387 |
| Same-owner claim recovery | 1.034 ms | 1.034–1.125 ms | 15,289 | 382 |
| Owned run get | 100.466 µs | 94.126–105.015 µs | 14,448 | 283 |
| Owned run list, 100 rows | 1.182 ms | 1.152–1.220 ms | 958,330 | 16,029 |
| Receipt lookup | 19.771 µs | 19.551–20.066 µs | 3,392 | 103 |
| SSE filter: 16 events | 37.661 µs | 36.923–38.548 µs | 82,871 | 315 |
| SSE filter: 4,096 events | 5.807 ms | 5.766–5.818 ms | 4,293,800 | 77,839 |
| Verified artifact acquisition + close | 315.948 ms | 305.862–368.699 ms | 437,154 | 3,166 |

### What is worth attention

1. **Artifact acquisition is expensive even on the tiny fixture.** Its full
   verification, filesystem copies, and Git subprocesses cost roughly a third of
   a second here. Avoid accidentally calling both `Inspect` and `Acquire` for one
   acquisition. The combined acquisition already avoids that duplication; these
   measurements do not justify weakening fresh integrity checks.
2. **The 100-row run list allocates about 936 KiB per call.** It is a credible
   profiling target if terminal/UI polling makes it frequent. A narrow read
   projection could help, but would need to preserve ownership and lifecycle
   semantics; a new generic repository layer would not address the allocation.
3. **SSE filtering allocates heavily across a batch.** The large fixture consumes
   roughly 4.1 MiB and 77,839 allocations across 4,096 events, not per event.
   The small fixture includes the worker/pipe/scanner startup cost. Profile real
   event distributions and concurrent attachments before pooling or parser work.
4. **Config parsing is allocation-heavy but startup-only.** About 66.5 µs and
   56 KiB per load is not evidence that it slows agent execution. A strict input
   size limit is a robustness concern independent of throughput.
5. **Cached authentication and readiness snapshots are not current targets.**
   Their local cost is small. Introducing duplicate atomic readiness state or
   a new authentication cache would add coordination without demonstrated need.

No implementation was rewritten during the original baseline measurement pass.

### Subsequent list allocation change

The list now uses capped geometric backing-array growth, starting at one row
and never reserving beyond the validated request limit. We rejected reserving
all 100 rows up front: although it saved 21.7% for full lists, it increased a
single-row result from 17,945 to 131,485 B/op.

Original versus selected implementation, medians of three local samples using
`-cpu=1 -count=3 -benchmem`:

| Fixture | Original B/op | Current B/op | Current allocs/op |
| --- | ---: | ---: | ---: |
| 100 rows | 958,330 | 900,985 | 16,029 |
| Empty, limit 100 | 10,544 | 10,544 | 121 |
| 1 row, limit 100 | 17,945 | 17,945 | 281 |
| 10 rows, limit 100 | 109,253 | 109,253 | 1,716 |

The full-list reduction is **6.0%**, with no sparse-result allocation regression.
Median full-list time was 1.137 ms versus 1.168 ms in the comparison run; sparse
times varied upward by roughly 2–5%. These are not statistically established
latency changes. The wide SQL projection and scanner remain the larger costs.
The new fixtures are `BenchmarkBackgroundRunListSparse` (see the package tests
for exact subtest names); run all taskstore benchmarks to reproduce the suite.

## What the fixtures measure

| Package | Timed work | Important exclusions |
| --- | --- | --- |
| `config` | Current YAML load/expansion, task policy decode, and validation as separate benchmarks | Config creation; no runtime startup or Docker |
| `control` | Cached device authentication with 1 and 64 devices | No expiry/pruning or hourly LastSeen persistence/fsync |
| `githubapp` | Strict decoding of a synthetic GitHub JSON response | HTTP/TLS, JWT signing, GitHub latency, token delivery |
| `strictjson` | Valid and invalid strict JSON checking, including a late duplicate key | No JSON canonical serialization; this package does not implement it |
| `observability` | Ready/blocked snapshots and parallel-reader benchmark with `-cpu=1` | HTTP transport; parallel readers do not model multicore scaling in this run |
| `taskstore` | Fresh admissions, same-owner claim recovery, owned reads/list100, receipt lookup | Database/workspace setup; admissions use fixture IDs, not secure entropy |
| `backgroundroute` | Full pipe/worker SSE filter, drain and close, 50% owned/foreign events | Network, TLS, upstream backpressure and slow clients |
| `taskartifact` | Fresh full Git/bundle verification, detached checkout and close | Initial snapshot/CAS installation; tiny fixture, not a large repository |

SQLite benchmarks keep production WAL, FULL synchronization, foreign keys, and
triggers. The fresh-admission benchmark grows its own database and includes
parameter construction; its average is not a constant-size database model.
Read fixtures contain 100 queued runs. Artifact measurements use real Git and
filesystem operations with warmed local caches and include checkout cleanup.

## Interpretation and limits

These are local baselines, not performance budgets, service-level objectives, or
before/after speedup claims. A single host cannot establish deployment-host or
large-repository scaling. The costs of Docker identity inspection, scoped-token
minting/delivery, real GitHub traffic, and model execution remain unmeasured.

Most other packages orchestrate I/O or enforce small bounded contracts. They
have source-level performance analysis in their READMEs, not invented timings.
Do not extrapolate cached authentication to token expiry, parser throughput to
HTTP throughput, or microseconds per store read to an entire agent workflow.

The largest relevant costs should be profiled with representative repositories
and actual workload traces before optimizing. In particular, do not TTL-cache
artifact verification or skip exact runtime identity checks to improve these
numbers: that would weaken the contract being measured.
