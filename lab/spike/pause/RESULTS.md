# Spike: freeze an idle box instead of stopping it

**Question:** today an idle box is stopped, and its next use starts every
process again: Postgres, the dev server, Claude. `runsc pause` freezes a
sandbox with its processes in memory. Does a paused box cost nothing, does it
come back as it was, and can its memory leave RAM meanwhile?

**Answer: yes to all three, in the local lab.** Run 2026-11-12 with
`lab/spike/pause/run.sh` (arm64, Docker Desktop's VM: kernel 6.10.14, 12
CPUs, 16 GB, a 2 GB swap file; runsc `release-20261012.0`), on boxes the
daemon made, paused and resumed behind its back with runsc. The lab's
machine is faster than the VPS: the times need measuring there before they
are quoted.

| | Result |
|---|---|
| `runsc pause` (10 runs) | 6 ms at the median, 12 ms at most |
| `runsc resume` | 9 ms at the median, 13 ms at most |
| Resume returned, until fern-init answers | 23 ms at the median, 41 ms at most |
| Postgres after ten pauses | the same process, its rows there |
| CPU of a paused box, over 10 s | 10 ms (an idle Astro dev server awake: 50 ms) |
| A command printing once a second, paused for 15 s | all 25 lines arrive, with a 16 s gap: the connection holds |
| The clocks in the box across a 20 s pause | both jump 20 s: wall clock and uptime keep counting |

## A heavy dev server's first page

An Astro site on Cloudflare's adapter (`lab/cloudflare-repo`), through the
preview, three runs each:

| From | First page |
|---|---|
| asleep (today) | 8.9, 9.2, 9.0 s |
| paused | 240, 242, 208 ms |
| paused, with all its memory pushed to swap | 831, 500, 448 ms |
| awake | 133 ms |

## Memory

Awake, that box holds 828 MiB (anon and shmem). Paused, it holds the same:
pausing alone frees CPU, not memory.

Writing to the cgroup's `memory.reclaim` while it is paused moves it to swap:
17 MiB stay in RAM and 680 MiB are in swap. After the resume and two pages,
71 to 78 MiB are back in RAM and the rest stays in swap: serving a page
touches a tenth of what the dev server allocated. A sandbox holding a 300 MB
buffer went from 341 MiB to 4 MiB the same way, and its first SQL after the
resume took 608 ms.

So a node needs swap for this, about as much as its paused boxes hold. The
kernel takes a paused box's pages first when memory is short, as they are the
coldest: the daemon need not push them out itself.

## What the daemon does with a box paused behind its back

A new command waits the 10 s of its dial, the daemon calls the box dead, and
stopping it and waking a new one takes 15 s more. Pausing has to be the
actor's own state, not something done beside it.

## Not shown

- **Claude across a pause.** The lab had no login. Claude between turns holds
  idle connections to its service, which will be dead after a long pause, and
  its clocks jump. It must be tried with a real turn before and after.
- **The VPS:** its times, whether it has swap (Hetzner's images come without),
  and a swap file's place (not on the pool's btrfs).
- **Long pauses** (hours), and a box's own open connections to the internet.
- **When to pause.** A box must not be paused in the middle of a turn: Claude
  waiting for its service uses no CPU either. The idle decision that
  hibernation makes from Claude's stream is still needed; only its stop and
  resume could go.

## What it would replace

If it holds on the VPS and for Claude: hibernation's stop and resume (and
the same work for Codex and pi), render-ahead and pre-wake, which exist to
hide a cold start. A revert, a fork and a box's first start stay cold, so
on-demand services and readiness stay.
