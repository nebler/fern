#!/usr/bin/env bash
# Checks Fern end to end in the lab, in sections:
#   slice        the whole disk, database included, is snapshotted and restored,
#                and a restore can be undone
#   box          a box runs under gVisor and keeps no state of its own: only
#                its workspace survives a sleep, and a killed box comes back
#   disk         DISK_HARDWARE=1 adds CPU/I/O profiling (all, not perf default)
#                a 100,000-file workspace: snapshot, restore (equal to its
#                snapshot) and rm (nothing left); a seeded create; a real clone
#   space        fern df names the pool, the disk under it and what uses
#                them; fern gc removes an image nothing runs from
#   quota        --env-disk-limit: a write past a thread's limit fails in its
#                box, fern doctor, fern df and its Claude say so, and fern
#                limit raising it lets the write through (restarts the
#                daemon; skipped on a VPS whose pool has no quotas on)
#   stream       fern exec keeps a stream intact: bytes, stderr, exit codes,
#                stdin, signals, cancellation
#   idle_exec    a command that prints nothing for 3 minutes still answers
#   concurrency  parallel execs on an awake and on a sleeping environment
#   restore      a restore while an exec is open; a missing snapshot
#   pgload       Postgres recovers, consistent, from every snapshot kept (the
#                newest 50) of 60 taken under pgbench's write load
#   idle         an idle box goes to sleep, with the daemon's idle times
#                shortened for the section; a running command keeps it awake
#   daemon       the daemon restarts while a box is awake, and finishes an rm
#                whose client went away
#   node         the whole node restarts (the lab's container; a VPS reboots)
#   many         ten environments woken at once
#   scale        awake boxes ramped up, 1, 5, 10, then 20 (SCALE): at each
#                step the new boxes' wakes, memory per box and the daemon's,
#                and exec and snapshot times with that many awake
#                SCALE_HARDWARE=0 disables the extra collectors for A/B runs
#   hardware     idle CPU with/without Postgres, platform overhead, pool data
#   stability    50 wake/sleep cycles and retained memory/FDs (all, not quick)
#   limits       what a box can't do: gain privileges, use more than its CPU
#                and PIDs, reach another box, the node or anything private
#   paths        workspaces at a path, as T3's worktrees: adopt, which,
#                exec --here, snapshot replace and delete
#   golden       a new worktree's checkout is built into a golden snapshot,
#                and the next worktree starts from it, its database seeded
#   tools        a TanStack Start repository pinning Node 22 and pnpm (mise)
#                gets a golden snapshot with them, and a thread of it serves
#                through a preview (needs the internet)
#   guess        the same repository with no Fern files and Postgres in
#                docker-compose.yml: Fern guesses its services and tools,
#                builds its golden and serves it (needs the internet)
#   cloudflare   an Astro site on Cloudflare with no Fern files: its guessed
#                dev server emulates D1 and R2, a revert brings their data
#                back, and the node's Cloudflare token reaches only the agent
#                box's Claude (needs the internet)
#   github       T3's git and gh get a token from the daemon's GitHub App,
#                against a fake GitHub on the node (restarts the daemon)
#   backup       a thread with Postgres backed up to an S3 store (versitygw
#                on the node), twice, and restored into another pool, the same
#                file for file (restarts the daemon; needs the internet)
#   upgrade      commands and a Bash call run on through a daemon restart, with
#                nothing lost or repeated, and a client that dies still ends
#                its command (restarts the daemon)
#   notify       a failed golden build is POSTed to --notify-url once, not
#                again after a restart, and so is the all-clear (restarts
#                the daemon; four minutes)
#   settings     settings and MCP servers planted in a workspace don't run in
#                the agent box; uses a fake login, so no subscription (with
#                agent boxes)
#   claude       Claude Code in a box: network, stream-json, its terminal UI
#   commit       Claude's commit tool commits a thread's changes on its
#                branch, on the node, where a box can't write to git; no hook
#                runs (with agent boxes)
#   devserver    a Vite dev server Claude starts in the background keeps
#                running after the call, hears the files Claude's box writes
#                and serves them, and runs on through a daemon restart (with
#                agent boxes; needs the internet; restarts the daemon)
#   t3           T3's server: the checkpoint hook's commands on a worktree it
#                made, and that worktree's box with git, Claude's folder and
#                login. Needs lab/build.sh t3.
# lab/check.sh runs the quick sections (slice box stream restore paths golden
# t3); lab/check.sh all runs every one; or name some: lab/check.sh stream
# daemon. Sections run side by side, JOBS (4) at a time, except those that
# restart things; JOBS=1 for timings undisturbed by the others. Needs
# lab/up.sh, or lab/vps.sh and FERN_HOST for a VPS.
# The sections are called by name, from main; ENV is ck-<section>, never
# words to split.
# What is set here is used in the sections' files, and theirs here: shellcheck
# reads each file by itself, so it can't see that (SC2034, and SC2154 there).
# shellcheck disable=SC2329,SC2086,SC2034
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
source lab/hardware.sh
# WAKES is how many times box sleeps and wakes its box, for the timings, and
# PREWAKES how many times it wakes it ahead.
ENV=ck IDLE=${IDLE:-180} WAKES=${WAKES:-10} PREWAKES=${PREWAKES:-5}
CLAUDE="$(sed -n 's/^ARG CLAUDE_VERSION=//p' images/Dockerfile) (Claude Code)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

FAILED=0

# The sections, a file each in lab/check/, and what they share (lib.sh).
source lab/check/lib.sh
for part in lab/check/*.sh; do
  # shellcheck source=/dev/null
  [ "$part" = lab/check/lib.sh ] || source "$part"
done

# main runs the sections. It is one function, called on the script's last
# line, so bash has read all of it before it starts: editing the script
# while it runs can't change what it does.
main() {
  all=(slice box disk space quota stream idle_exec concurrency restore fork pgload idle daemon upgrade settings node many scale sleeping limits sizes paths golden tools guess next cloudflare backup github claude opencode commit devserver turn hardware stability notify)
  quick=(slice box stream restore paths golden t3)
  # These restart the daemon or the node, change the daemon's arguments or
  # load the node with many boxes, so they run alone, after the others.
  alone=" idle daemon upgrade quota sizes node many scale sleeping turn hardware stability github devserver backup notify "
  case "${1:-}" in
    "") set -- "${quick[@]}" ;;
    all) set -- "${all[@]}" t3 && WAKES=${WAKES_ALL:-30} && DISK_HARDWARE=1 ;;
  esac
  # Claude in a box of its own (lab/boxes.sh agents): <env>.agent.
  if agent_boxes; then export AGENTS=1; else export AGENTS=0; fi
  for s in "$@"; do
    case " ${all[*]} t3 " in
      *" $s "*) ;;
      *) echo "no section $s; the sections are: ${all[*]} t3 (or quick, the default, or all)" >&2; exit 2 ;;
    esac
  done

  # run_section runs section $1 with an environment and a temporary folder of
  # its own, so sections can run side by side, and prints its output and how
  # long it took when it is done. Its status is 1 if a check failed.
  run_section() {
    local s=$1 start status=0
    local output=$TMP/$s.out
    # One by one, for timings: btrfs first finishes removing what earlier
    # sections deleted, which a later snapshot would otherwise wait for
    # (lab/RESULTS.md, "Baselines with the hardware metrics").
    if [ "${JOBS:-4}" = 1 ]; then in_daemon "btrfs subvolume sync /var/lib/fern" >/dev/null; fi
    start=$(now_ms)
    # shellcheck disable=SC2030,SC2031 # the section's own, in its subshell
    (
      ENV=ck-${s//_/-} TMP=$TMP/$s
      mkdir -p "$TMP"
      "$s"
      fern rm "$ENV" 2>/dev/null || true
      exit "$FAILED"
    ) >"$output" 2>&1 || status=1
    # shellcheck disable=SC2031
    echo "$s $(($(now_ms) - start))" >>"$TMP/times"
    # shellcheck disable=SC2031
    cat "$output"
    if grep -q '^FAIL ' "$output"; then status=1; fi
    return $status
  }

  # Up to JOBS sections at a time (default 4); JOBS=1 runs them one by one,
  # for timings undisturbed by the others.
  jobs=${JOBS:-4} failed=0
  start=$(now_ms)
  for s in "$@"; do
    case "$alone" in *" $s "*) continue ;; esac
    while [ "$(jobs -rp | wc -l)" -ge "$jobs" ]; do wait -n || failed=1; done
    run_section "$s" &
  done
  while [ -n "$(jobs -rp)" ]; do wait -n || failed=1; done
  for s in "$@"; do
    case "$alone" in *" $s "*) run_section "$s" || failed=1 ;; esac
  done
  echo "== time per section, ms (side by side, so they add up to more than the run)"
  # shellcheck disable=SC2031
  sort -k2 -n -r "$TMP/times" | awk '{ printf "  %-12s %7d\n", $1, $2 }'
  echo "  total        $(printf %7d $(($(now_ms) - start)))"
  FAILED=$failed
  if [ "$FAILED" = 0 ]; then echo "check: PASS"; else echo "check: FAIL"; exit 1; fi
}

main "$@"; exit
