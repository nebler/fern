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
# shellcheck disable=SC2329,SC2086
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
# check NAME WANT GOT records one pass or fail.
check() {
  if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: want '$2', got '$3'"; FAILED=1; fi
}
note() { echo "NOTE $*"; }
# metric KEY TEXT VALUE notes "TEXT: VALUE" and, with METRICS set (by
# lab/perf.sh, which compares it with a baseline), records VALUE under KEY:
# lower is better, and KEY ends in its unit (_ms, _mib, _bytes, _count).
# A VALUE from stats records its p50 and p95. Node memory deltas can be
# negative when unrelated memory is reclaimed during a scale step.
metric() {
  if [[ $3 != n=* && ! $3 =~ ^-?[0-9]+([.][0-9]+)?([[:space:]].*)?$ ]]; then
    echo "FAIL metric $1: invalid value '$3'" >&2
    FAILED=1
    return 1
  fi
  note "$2: $3"
  [ -n "${METRICS:-}" ] || return 0
  case $3 in
    n=*) for p in p50 p95; do echo "METRIC $1.$p $(sed -nE "s/.* $p=([0-9.]+).*/\1/p" <<<"$3")"; done ;;
    *) echo "METRIC $1 ${3%% *}" ;;
  esac
}
# succeeds prints yes if the command succeeds, no if it fails.
succeeds() { if "$@" >/dev/null 2>&1; then echo yes; else echo no; fi; }
# now_ms prints the wall clock in milliseconds.
now_ms() { local t=${EPOCHREALTIME//[!0-9]/}; echo $((t / 1000)); }
# node_ms runs a command on the node and prints how long it took there, ms.
# Short commands are timed where they run: the way to the node (SSH to a VPS
# from a Mac, 80-160 ms and jittery) isn't Fern's time.
node_ms() {
  # shellcheck disable=SC2016 # expanded on the node
  on_node sh -c 's=$(date +%s%N); "$@" >/dev/null || exit; echo $((($(date +%s%N) - s) / 1000000))' sh "$@" </dev/null
}
# took reads the time from fern's "… took 12.3ms".
took() { sed -nE 's/.* took ([0-9.]+)ms$/\1/p'; }
# stats reads numbers, one per line, and prints "n=… p50=… p95=… max=…",
# nearest-rank percentiles, and up to 30 of them all, sorted. Under 20
# numbers p95 is the maximum.
stats() {
  sort -n | awk '
    function rank(p,  i) { i = int(p * NR); if (i < p * NR) i++; return a[i] }
    {a[NR]=$1} END {
      if (NR == 0) { print "n=0"; exit }
      printf "n=%d p50=%s p95=%s max=%s", NR, rank(0.50), rank(0.95), a[NR]
      if (NR <= 30) { printf " all:"; for (i = 1; i <= NR; i++) printf " %s", a[i] }
      print "" }'
}
# metrics prints the daemon's Prometheus metrics.
metrics() { on_node curl -sf http://127.0.0.1:9090/metrics </dev/null; }
# counter_since prints how much counter $2 in the metrics on stdin grew
# since the metrics in file $1 were read.
counter_since() {
  awk -v c="$2" 'NR == FNR { if ($1 == c) b = $2; next } $1 == c { a = $2 } END { printf "%d", a - b }' "$1" -
}
# wake_steps prints the mean time of each step of the wakes since the
# metrics in file BEFORE were read.
wake_steps() {
  { sed 's/^/before /' "$1"; metrics | sed 's/^/after /'; } | awk '
    $2 ~ /^fern_wake_step_seconds_(sum|count)/ {
      match($2, /step="[a-z]+"/); step = substr($2, RSTART + 6, RLENGTH - 7)
      kind = ($2 ~ /_sum/) ? "sum" : "count"
      v[$1, kind, step] = $3; steps[step] = 1 }
    END { for (s in steps) {
      n = v["after", "count", s] - v["before", "count", s]
      if (n > 0) printf "%s=%.0f ", s, 1000 * (v["after", "sum", s] - v["before", "sum", s]) / n }
      print "(mean ms)" }'
}
sha256() { shasum -a 256 | cut -d' ' -f1; }
sql() { fern exec "$1" -- psql -tAc "$2"; }
# state prints an environment's state, or nothing if there is no such
# environment.
state() { fern ls | awk -v e="$1" '$1 == e { print $2 }'; }
# fresh makes an environment from the sample repo, replacing any of that name.
fresh() { fern rm "$1" 2>/dev/null || true; fern create "$1" --repo lab/sample-repo >/dev/null; }
# asleep_within prints "asleep" once environment $1 is, or its state after
# $2 seconds: a box takes a moment to stop, longer under gVisor.
asleep_within() {
  local s
  for _ in $(seq $(($2 * 5))); do
    s=$(state "$1")
    [ "$s" = asleep ] && break
    sleep 0.2
  done
  echo "$s"
}
# pod_cgroup prints file $2 of environment $1's box's cgroup on the node,
# which the daemon sets and the box can't see.
pod_cgroup() {
  pod_cgroups "$2" "$(pod_uid "$1")"
}
# pod_cgroups reads file $1 for the boxes (box_uid) that follow, in one node
# call.
pod_cgroups() {
  on_node sh -s -- "$@" <<'NODE'
file=$1
shift
for uid do
  box=${uid#runsc-}
  cat "/sys/fs/cgroup/fern/${box%@*}/$file" || exit 1
done
NODE
}
# node_used counts shared application memory too: BusyBox free's used column
# subtracts Cached (including shmem), while procps free uses MemAvailable.
node_used() {
  # shellcheck disable=SC2016 # awk fields, not shell variables
  on_node awk '/^MemTotal:/ { total = $2 } /^MemAvailable:/ { available = $2 }
    END { printf "%.3f\n", (total - available) / 1024 }' /proc/meminfo </dev/null
}
# golden_id prints the ID of the environment that builds repo $1's golden
# snapshots (store.BuilderID), for a name of lowercase letters, digits and dashes.
golden_id() { echo "golden-$1-$(printf %s "$1" | shasum -a 256 | cut -c1-8)"; }
# in_daemon runs a shell command on the node, where the daemon runs and
# sees the pool.
in_daemon() { on_node sh -c "$1"; }
# pod_uid prints what identifies environment $1's running box (box_uid).
pod_uid() { box_uid "$1"; }
# kill_box kills environment $1's box as a crash would: its sandbox killed.
kill_box() {
  local pid
  pid=$(pod_uid "$1")
  on_node kill -KILL "${pid#*@}" </dev/null >/dev/null 2>&1
}
# box_ip prints environment $1's box's address.
box_ip() {
  in_daemon "cat /run/fern/boxes/$1/net" | awk '{ n = $1 * 4 + 2; printf "10.89.%d.%d\n", int(n / 256), n % 256 }'
}
# box_uids prints the boxes' identities (pod_uid) of the environments named
# $1 followed by a number up to $2.
box_uids() {
  local i
  for i in $(seq "$2"); do pod_uid "$1$i"; done | tr '\n' ' '
}
# restart_node restarts the node, and with it every box: the lab's
# container. A VPS loses power instead: its services stop and every box is
# killed, as at a crash, then they start again. A real reboot (REBOOT=1)
# risks a machine that doesn't come back without its provider's console.
restart_node() {
  if [ -n "$FERN_HOST" ] && [ "${REBOOT:-}" = 1 ]; then
    on_node systemctl reboot </dev/null || true
    sleep 15
    local i
    for i in $(seq 150); do
      lab_up 2>/dev/null && break
      [ "$i" = 150 ] && { echo "the VPS isn't back 5 minutes after its reboot" >&2; return 1; }
      sleep 2
    done
  elif [ -n "$FERN_HOST" ]; then
    on_node sh -c 'systemctl kill -s KILL fern-daemon fern-t3; systemctl stop fern-daemon fern-t3
      echo 1 >/sys/fs/cgroup/fern/cgroup.kill
      systemctl start fern-daemon; systemctl is-enabled -q fern-t3 && systemctl start fern-t3' </dev/null || true
  else
    docker restart "$LAB" >/dev/null
  fi
  wait_daemon
}

slice() {
  echo "== the whole disk, database included, is snapshotted and restored"
  fresh $ENV
  check "a new environment is asleep" asleep "$(state $ENV)"
  check "the first exec wakes its box (3 users)" 3 "$(sql $ENV 'select count(*) from users')"
  fern snapshot $ENV s1 >/dev/null
  fern exec $ENV -- psql -qc 'drop table users'
  check "the table is gone" 0 "$(sql $ENV "select count(*) from pg_tables where tablename = 'users'")"
  fern restore $ENV s1 >/dev/null
  check "a restore leaves it asleep" asleep "$(state $ENV)"
  check "the next exec wakes it with the table (3 users)" 3 "$(sql $ENV 'select count(*) from users')"
  local pre
  pre=$(fern snapshots $ENV | awk '$1 ~ /^pre-restore-/ { print $1 }')
  check "a restore keeps the workspace it replaced" 1 "$(grep -c . <<<"$pre")"
  fern restore $ENV "$pre" >/dev/null
  check "restoring that undoes the restore (no table)" 0 \
    "$(sql $ENV "select count(*) from pg_tables where tablename = 'users'")"
  fern rm $ENV
  check "rm removes it" "" "$(state $ENV)"
}

# mounts counts environment $1's box's mounts on the node: its root's.
mounts() { on_node grep -c "/boxes/$1/rootfs " /proc/self/mountinfo </dev/null || true; }
# procs prints the names of the processes in an environment's box.
procs() { fern exec "$1" -- sh -c 'cat /proc/[0-9]*/comm 2>/dev/null; true' | sort -u; }

box() {
  echo "== a box keeps no state of its own"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Edsger', 'edsger@example.com')" >/dev/null
  # setsid: an exec's own process group ends with it. The pause lets the
  # child leave the group before the exec ends and the group is killed.
  fern exec $ENV -- sh -c 'echo x > /tmp/outside && echo y > inside && setsid -f sleep 1000 >/dev/null 2>&1 && sleep 0.5'
  check "the box runs under gVisor" "Starting gVisor..." "$(fern exec $ENV -- dmesg | head -1 | cut -c16-)"
  check "the daemon runs the box with runsc" yes "$([ -n "$(pod_uid $ENV)" ] && echo yes || echo no)"
  check "the root filesystem is read-only" no "$(succeeds fern exec $ENV -- touch /etc/x)"
  # The C library's lookup, as Node's fetch makes it: curl knows localhost itself.
  check "localhost is the loopback address" 127.0.0.1 "$(fern exec $ENV -- getent ahostsv4 localhost | awk 'NR == 1 { print $1 }')"
  check "the box writes to the pool" y "$(in_daemon "cat /var/lib/fern/envs/$ENV/live/inside")"
  # What the node is doing now, for a person at the server: the box, how
  # long it has been up, its memory and CPU time, and what holds it awake.
  check "fern top lists the box, with what holds it" 1 "$(fern top | grep -cE "^$ENV +services +[0-9.smhd]+ +[0-9.]+ MiB +[0-9.smhd]+ +[^?]" || true)"
  check "a stray process runs" 1 "$(procs $ENV | grep -cx sleep || true)"
  check "fern why says the box is awake, and when it sleeps" "awake in" "$(fern why $ENV | awk -v e=$ENV '$1 == e { print $2, $4 }')"
  local mounted=0
  metrics >"$TMP/metrics"
  for _ in $(seq "$WAKES"); do
    fern sleep $ENV | took >>"$TMP/sleep"
    mounted=$((mounted + $(mounts $ENV)))
    fern wake $ENV | took >>"$TMP/wake"
  done
  metric box.sleep_ms "sleep (until the box is gone), ms" "$(stats <"$TMP/sleep")"
  metric box.wake_ms "wake (until Postgres is ready), ms" "$(stats <"$TMP/wake")"
  note "wake steps: $(wake_steps "$TMP/metrics")"
  # Memory without the page cache, which comes and goes: the box's (Postgres
  # idle, gVisor included) and the daemon's. gVisor keeps what the box's
  # programs allocate in shared memory, so the box's counts shmem too.
  metric box.box_memory_mib "an awake box's memory, MiB" \
    "$(pod_cgroup $ENV memory.stat | awk '$1 == "anon" || $1 == "shmem" { b += $2 } END { printf "%.1f", b / 1048576 }')"
  metric box.daemon_memory_mib "the daemon's memory, MiB" \
    "$(in_daemon 'cat /proc/1/status' | awk '$1 == "RssAnon:" { printf "%.1f", $2 / 1024 }')"
  check "the workspace is unmounted at every sleep" 0 "$mounted"
  check "and mounted once while awake" 1 "$(mounts $ENV)"
  check "a write outside the workspace is gone" no "$(succeeds fern exec $ENV -- test -e /tmp/outside)"
  check "a write in it stays" y "$(fern exec $ENV -- cat inside)"
  check "and so does the database (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
  check "only the declared services come back" 0 "$(procs $ENV | grep -cx sleep || true)"
  # T3 asks for a pre-wake when a thread is opened or reverted. It answers
  # at once; a command after the user's think time (3 s here) finds the box
  # up. Interleaved with commands to an asleep box, which wait for the wake.
  metrics >"$TMP/metrics"
  for _ in $(seq "$PREWAKES"); do
    fern sleep $ENV >/dev/null
    fern prewake $ENV | took >>"$TMP/prewake"
    sleep 3
    # With agent boxes a pre-wake wakes Claude's: the next turn's command.
    if [ "${AGENTS:-0}" = 1 ]; then
      node_ms "$FERN_CLI" exec --agent $ENV -- true >>"$TMP/prewoken"
    else
      node_ms "$FERN_CLI" exec $ENV -- true >>"$TMP/prewoken"
    fi
    fern sleep $ENV >/dev/null
    node_ms "$FERN_CLI" exec $ENV -- true >>"$TMP/asleep"
  done
  metric box.prewake_ms "a pre-wake's answer, ms" "$(stats <"$TMP/prewake")"
  metric box.prewoken_exec_ms "a command 3 s after a pre-wake, ms" "$(stats <"$TMP/prewoken")"
  metric box.asleep_exec_ms "a command to an asleep box, ms" "$(stats <"$TMP/asleep")"
  check "every pre-wake was used by its command" "$PREWAKES" "$(metrics | counter_since "$TMP/metrics" 'fern_prewakes_total{result="used"}')"
  sql $ENV "insert into users (name, email) values ('Barbara', 'barbara@example.com')" >/dev/null
  local users=""
  start=$(now_ms)
  kill_box $ENV
  # The box comes back with the next command, which finds the old one gone.
  for _ in $(seq 80); do
    users=$(sql $ENV 'select count(*) from users' 2>/dev/null) && break
    sleep 0.25
  done
  check "Postgres recovers after its box is killed (5 users)" 5 "$users"
  metric box.force_delete_recovery_ms "kill until the new box's Postgres answers, ms" $(($(now_ms) - start))
  check "and the seed ran once" 1 "$(sql $ENV "select count(*) from users where name = 'Ada'")"
  # That was its first crash; at the third in five minutes it is given up on.
  kill_box $ENV
  fern exec $ENV -- true >/dev/null 2>&1
  kill_box $ENV
  check "a box that crashed 3 times in 5 minutes isn't woken again" yes \
    "$(succeeds grep -q 'keeps crashing' <<<"$(fern exec $ENV -- true 2>&1)")"
  check "until a person wakes it" 0 "$(fern wake $ENV >/dev/null 2>&1; fern exec $ENV -- true >/dev/null 2>&1; echo $?)"
}

# treehash hashes the path and content of every file in a folder of the pool.
treehash() { in_daemon "cd '$1' && find . -type f -print0 | sort -z | xargs -0r sha256sum | sha256sum | cut -c1-16"; }

disk() {
  echo "== a big workspace"
  local D=/var/lib/fern/envs/big
  local -a snapshot=(fern snapshot) restore_disk=(fern restore)
  # Resource reads alter the I/O cadence before seeded-create. Profile
  # these large-workspace operations only in all or explicit CPU runs.
  if [ "${DISK_HARDWARE:-0}" = 1 ]; then
    snapshot=(resource_op disk.snapshot snapshot)
    restore_disk=(resource_op disk.restore restore)
  fi
  fern rm big 2>/dev/null || true
  fern create big >/dev/null
  # 1,000 folders of 100 small files, as node_modules has, and a 1 GB file.
  in_daemon "cd $D/live && mkdir -p node_modules/p0 && cd node_modules &&
    for f in \$(seq 100); do head -c 1024 /dev/urandom > p0/f\$f; done &&
    for d in \$(seq 999); do cp -r --reflink=never p0 p\$d; done &&
    head -c 1000000000 /dev/urandom > ../blob && chown -R 1000:1000 $D/live && sync"
  note "workspace: $(in_daemon "find $D/live | wc -l") files, $(in_daemon "du -sh $D/live | cut -f1")"
  for i in $(seq 10); do "${snapshot[@]}" big "s$i" | took; done >"$TMP/snap"
  metric disk.snapshot_ms "snapshot, ms" "$(stats <"$TMP/snap")"
  in_daemon "echo changed > $D/live/blob && rm $D/live/node_modules/p0/f1 && echo new > $D/live/new"
  for i in $(seq 5); do "${restore_disk[@]}" big "s$i" | took; done >"$TMP/restore"
  metric disk.restore_ms "restore (asleep, so no box to stop), ms" "$(stats <"$TMP/restore")"
  check "a restore equals its snapshot (tree hash)" "$(treehash $D/snaps/s1)" "$(treehash $D/live)"
  start=$(now_ms)
  fern rm big
  metric disk.rm_ms "rm with 10 snapshots, ms" $(($(now_ms) - start))
  check "rm leaves nothing behind" no "$(succeeds in_daemon "test -e $D")"

  python3 -c '
import os, sys
for d in range(200):
    os.makedirs(f"{sys.argv[1]}/d{d}")
    for f in range(100):
        open(f"{sys.argv[1]}/d{d}/f{f}", "wb").write(os.urandom(1024))' "$TMP/repo"
  start=$(now_ms)
  fern create seeded --repo "$TMP/repo" >/dev/null
  metric disk.create_seeded_ms "create seeded from the Mac with 20,000 files (20 MB), ms" $(($(now_ms) - start))
  fern rm seeded

  fresh $ENV
  start=$(now_ms)
  if fern exec $ENV -- git clone -q --depth 1 https://github.com/pingdotgg/t3code.git t3code; then
    note "git clone --depth 1 of t3code in a box: $(($(now_ms) - start)) ms," \
      "$(fern exec $ENV -- sh -c 'find t3code | wc -l') files, $(fern exec $ENV -- du -sh t3code | cut -f1)"
    note "snapshot with it: $(fern snapshot $ENV c1 | took) ms"
  else
    note "git clone of t3code failed"
  fi
}

# space checks fern df and fern gc on a node with an image a tag has moved
# off, which nothing runs from.
space() {
  echo "== what uses the disk, and what nothing needs"
  local old
  old=$(printf 'e%.0s' $(seq 64))
  fresh $ENV
  fern exec $ENV -- sh -c 'head -c 3000000 /dev/urandom > blob'
  fern snapshot $ENV s1 >/dev/null
  in_daemon "mkdir -p /var/lib/fern/images/$old/root && head -c 1000000 /dev/urandom > /var/lib/fern/images/$old/root/f"
  fern df >"$TMP/df"
  check "df shows the pool" yes "$(succeeds grep -q '^the pool .*/var/lib/fern' "$TMP/df")"
  check "df shows the server's disk, which the pool is a file on" yes "$(succeeds grep -q "^the server's disk .*/var/lib/fern-img" "$TMP/df")"
  check "df lists the environment" yes "$(succeeds grep -q "^environment $ENV " "$TMP/df")"
  check "df says gc removes an image nothing runs from" yes "$(succeeds grep -q "^image ${old:0:12} .*fern gc removes it" "$TMP/df")"
  if in_daemon "command -v podman" >/dev/null 2>&1; then
    note "fern gc skipped: on this node it would remove Podman's images, the next build's cache"
    in_daemon "rm -rf /var/lib/fern/images/$old"
    return
  fi
  fern gc >"$TMP/gc"
  check "gc removes the image nothing runs from" no "$(succeeds in_daemon "test -e /var/lib/fern/images/$old")"
  check "gc says what it removed" yes "$(succeeds grep -q "^removed image ${old:0:12}" "$TMP/gc")"
  check "gc keeps the box image" yes "$(succeeds in_daemon "test -e /var/lib/fern/images/fern-box:dev/root")"
  check "the environment still wakes" hi "$(fern exec $ENV -- echo hi)"
}

stream() {
  echo "== the exec stream"
  fresh $ENV
  fern exec $ENV -- true
  python3 -c 'import json; [print(json.dumps({"type": "user", "n": i, "text": "é☃" * 20})) for i in range(10000)]' >"$TMP/lines"
  check "10,000 JSON lines through cat, unchanged" "$(sha256 <"$TMP/lines")" "$(fern exec -i $ENV -- cat <"$TMP/lines" | sha256)"
  head -c 50000000 /dev/urandom >"$TMP/bin"
  start=$(now_ms)
  got=$(fern exec -i $ENV -- cat <"$TMP/bin" | sha256)
  metric stream.roundtrip_50mb_ms "50 MB round trip (Mac -> box -> Mac), ms" $(($(now_ms) - start))
  check "50 MB of random bytes through cat, unchanged" "$(sha256 <"$TMP/bin")" "$got"
  # Counted on the node: the Mac's link to a VPS is too slow to measure Fern by.
  start=$(now_ms)
  n=$(in_daemon "fern exec $ENV -- head -c 200000000 /dev/zero | wc -c" | tr -d ' ')
  metric stream.stdout_200mb_ms "200 MB of stdout, ms" $(($(now_ms) - start))
  check "200 MB of stdout, every byte" 200000000 "$n"
  check "stdout and stderr stay apart" out "$(fern exec $ENV -- sh -c 'echo out; echo err >&2' 2>/dev/null)"
  check "the exit code passes through" 42 "$(fern exec $ENV -- sh -c 'exit 42' >/dev/null 2>&1; echo $?)"
  check "stdin's end reaches the command" 3 "$(printf 'a\nb\nc\n' | fern exec -i $ENV -- wc -l | tr -d ' ')"
  # Endless input: a command that got it would never end.
  check "without -i the command gets no stdin" yes "$(succeeds in_daemon "timeout 20 fern exec $ENV -- cat </dev/zero")"
  rc=$(in_daemon "(sleep 2; printf '\003') | script -qec 'fern exec -it $ENV -- sleep 30' /dev/null >/dev/null 2>&1; echo \$?" || true)
  check "Ctrl-C in a terminal stops the command (130)" 130 "$rc"
  # A full-screen program, as claude is, gets the terminal raw: Ctrl-C is a byte.
  out=$(in_daemon "(sleep 2; printf '\003\003'; sleep 3) |
    timeout 4 script -qec \"fern exec -it $ENV -- sh -c 'stty raw -echo; dd bs=1 count=2 2>/dev/null | od -An -c'\" /dev/null" || true)
  check "and reaches a program that reads the terminal raw" "003 003" "$(tr -d '\r' <<<"$out" | xargs)"
  out=$(in_daemon "fern exec $ENV -- sh -c 'trap \"echo stopping; exit 7\" TERM; echo ready; while :; do sleep 0.1; done' 2>/dev/null &
    sleep 2; kill -TERM \$!; wait \$!; echo code=\$?")
  check "SIGTERM to fern exec reaches the command (exit 7)" "ready stopping code=7" "${out//$'\n'/ }"
  in_daemon "timeout 3 fern exec $ENV -- sleep 301 || true"
  sleep 2
  check "a client that goes away stops its command" 0 \
    "$(fern exec $ENV -- sh -c 'cat /proc/[0-9]*/cmdline 2>/dev/null | tr "\0" " "' | grep -c 'sleep 301' || true)"
}

# idle_exec: a command that prints nothing for IDLE seconds (180) still
# answers: nothing between the client and the box drops an idle stream.
idle_exec() {
  echo "== an idle exec stream"
  fresh $ENV
  check "a command idle for ${IDLE}s still answers" alive "$(fern exec $ENV -- sh -c "sleep $IDLE; echo alive")"
}

concurrency() {
  echo "== concurrency"
  fresh $ENV
  fern exec $ENV -- true
  for i in $(seq 10); do fern exec $ENV -- echo ok >"$TMP/awake$i" 2>&1 & done
  wait
  check "10 parallel execs on an awake box" 10 "$(cat "$TMP"/awake* | grep -cx ok || true)"
  fern sleep $ENV >/dev/null
  start=$(now_ms)
  for i in $(seq 10); do sql $ENV 'select count(*) from users' >"$TMP/asleep$i" 2>&1 & done
  wait
  metric concurrency.parallel_execs_ms "10 parallel execs on a sleeping box, ms in all" $(($(now_ms) - start))
  check "all 10 wake it and see the database" 10 "$(cat "$TMP"/asleep* | grep -cx 3 || true)"
}

restore() {
  echo "== restore"
  fresh $ENV
  fern exec $ENV -- true
  fern snapshot $ENV r1 >/dev/null
  start=$(now_ms)
  fern exec $ENV -- sh -c 'sleep 120; echo finished' >"$TMP/open" 2>&1 &
  local open=$! code=0
  sleep 2
  fern restore $ENV r1 >/dev/null
  wait $open || code=$?
  note "the open exec ended after $(($(now_ms) - start)) ms, exit $code: $(tr '\n' ' ' <"$TMP/open")"
  check "a restore ends an open exec (no hang, not 'finished')" 0 "$(grep -c finished "$TMP/open" || true)"
  check "the environment works after it (3 users)" 3 "$(sql $ENV 'select count(*) from users')"
  check "restoring a missing snapshot is refused" no "$(succeeds fern restore $ENV no-such-snapshot)"
  check "and leaves the box running" running "$(state $ENV)"
}

# fork: three forks of a thread, taken while its Postgres runs, run their
# "tests" at the same time, each against its own Postgres on :5432
# (roadmap step 7), and go with the thread.
fork() {
  echo "== fork"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Fern', 'fern@example.com')" >/dev/null
  fern fork -n 3 $ENV >"$TMP/forks" 2>"$TMP/took"
  check "three forks" "$ENV-fork-1 $ENV-fork-2 $ENV-fork-3" "$(xargs <"$TMP/forks")"
  metric fork.three_ms "three forks of a running thread, ms" "$(took <"$TMP/took")"
  local start i
  start=$(now_ms)
  for i in 1 2 3; do
    # Fork i adds i users, then counts them, and its Postgres's port.
    fern exec $ENV-fork-$i -- sh -c "psql -qc \"insert into users (name, email) select 'f$i', 'x' from generate_series(1, $i)\" &&
      psql -tAc 'select count(*) from users' && psql -tAc 'show port'" >"$TMP/fork$i" 2>&1 &
  done
  wait
  metric fork.tests_ms "three forks woken and tested at once, ms" "$(($(now_ms) - start))"
  for i in 1 2 3; do
    check "fork $i has its own database (4 + $i users) on :5432" "$((4 + i)) 5432" "$(xargs <"$TMP/fork$i")"
  done
  check "the thread's database is untouched (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
  if [ "$AGENTS" = 1 ]; then
    # Claude's tools, from its agent box, as Claude calls them.
    check "env_fork" "Forks: $ENV-fork-4, $ENV-fork-5" "$(mcp $ENV env_fork '{"count": 2}')"
    check "env_exec runs in a fork (4 users)" 4 "$(mcp $ENV env_exec "{\"env\": \"$ENV-fork-4\", \"command\": \"psql -tAc 'select count(*) from users'\"}")"
    check "env_exec refuses the thread itself" no "$(succeeds mcp $ENV env_exec "{\"env\": \"$ENV\", \"command\": \"true\"}")"
    check "env_delete" "Deleted $ENV-fork-5." "$(mcp $ENV env_delete "{\"env\": \"$ENV-fork-5\"}")"
  fi
  check "more than 8 forks are refused" no "$(succeeds fern fork -n 9 $ENV)"
  fern rm $ENV
  check "deleting the thread deletes its forks" 0 "$(fern ls | grep -c "^$ENV-fork-" || true)"
}

# sleeping: how many asleep threads a node holds. A thread with Postgres
# forks into 8, each of those into 8, and so on: 585 environments that share
# their data as threads from one golden do, each with a Postgres data folder
# of its own. Asleep, a thread is files and a row; this measures what they
# cost the disk, the daemon and its answers, and a wake among them.
sleeping() {
  echo "== sleeping threads"
  fresh $ENV
  sql $ENV 'select count(*) from users' >/dev/null
  fern sleep $ENV >/dev/null
  in_daemon "btrfs filesystem sync /var/lib/fern" >/dev/null
  local disk0 rss0 start total
  disk0=$(in_daemon "df --output=used -B1M /var/lib/fern | tail -1" | tr -d ' ')
  # The process's own memory: its cgroup's also counts the page cache its
  # snapshots read.
  rss0=$(in_daemon 'ps -o rss= -p "$(systemctl show -P MainPID fern-daemon)"')
  start=$(now_ms)
  # On the node: one SSH session, not 73.
  # shellcheck disable=SC2016 # the node's shell's
  on_node bash -c 'set -e; level=("$1"); for _ in 1 2 3; do next=(); for e in "${level[@]}"; do
      mapfile -t -O "${#next[@]}" next < <("$2" fork -n 8 "$e" 2>/dev/null); done; level=("${next[@]}"); done' bash $ENV "$FERN_CLI" </dev/null
  total=$(fern ls | grep -c "^$ENV" || true)
  check "585 threads (1 + 8 + 64 + 512)" 585 "$total"
  metric sleeping.fork584_ms "584 forks made, ms" $(($(now_ms) - start))
  in_daemon "btrfs filesystem sync /var/lib/fern" >/dev/null
  metric sleeping.disk_per_thread_kib "disk per asleep thread, KiB (metadata: its data is shared until written)" \
    "$(($(($(in_daemon "df --output=used -B1M /var/lib/fern | tail -1" | tr -d ' ') - disk0)) * 1024 / 584))"
  metric sleeping.daemon_memory_mib "the daemon's memory (RSS) with 585 asleep threads, MiB more than with 1" \
    "$(awk -v a="$(in_daemon 'ps -o rss= -p "$(systemctl show -P MainPID fern-daemon)"')" -v b="$rss0" 'BEGIN { printf "%.1f", (a - b) / 1024 }')"
  metric sleeping.ls_ms "fern ls with 585 threads, ms" "$(node_ms "$FERN_CLI" ls)"
  local deep=$ENV-fork-5-fork-3-fork-7
  metric sleeping.wake_ms "a wake among them, ms" "$(fern wake $deep | took)"
  check "it has its own Postgres with the data (3 users)" 3 "$(sql $deep 'select count(*) from users')"
  start=$(now_ms)
  restart_daemon
  metric sleeping.restart_ms "daemon restart with 585 threads, ms until it answers" $(($(now_ms) - start))
  check "the threads are all there after it" 585 "$(fern ls | grep -c "^$ENV" || true)"
  start=$(now_ms)
  fern rm $ENV
  metric sleeping.delete585_ms "the thread and its 584 forks deleted, ms" $(($(now_ms) - start))
  check "none is left" 0 "$(fern ls | grep -c "^$ENV" || true)"
}

# pg_load ENV N takes N snapshots of ENV's Postgres, one a second, while
# pgbench writes to it, then restores each and checks that Postgres starts,
# pg_amcheck finds no corruption, and every transaction is whole: pgbench
# moves each amount between an account, a teller and a branch, so their
# balances sum to the same.
pg_load() {
  local env=$1 n=$2 ok=0
  fern exec "$env" -- pgbench -iq -s 5 >/dev/null 2>&1
  fern exec "$env" -- pgbench -n -c 4 -T $((n + 10)) >"$TMP/pg-load-$env" 2>&1 &
  local load=$!
  sleep 2
  for i in $(seq "$n"); do
    fern snapshot "$env" "p$i" | took >>"$TMP/pg-snap-$env"
    sleep 1
  done
  # run_section collects failures with ||, which disables errexit here.
  if ! wait "$load"; then
    check "pgbench write load completes" yes no
    cat "$TMP/pg-load-$env" >&2
    return
  fi
  metric pgload.latency_ms "pgbench write latency under snapshots, ms" \
    "$(awk '$1 == "latency" && $2 == "average" { print $4 }' "$TMP/pg-load-$env")"
  local whole="select (select sum(abalance) from pgbench_accounts) = (select sum(bbalance) from pgbench_branches)
    and (select sum(bbalance) from pgbench_branches) = (select sum(tbalance) from pgbench_tellers)"
  # The daemon keeps an environment's newest 50 snapshots (store.keepTurns).
  local first=$((n > 50 ? n - 49 : 1))
  if [ "$first" -gt 1 ]; then
    check "the oldest snapshots were pruned, keeping the newest 50" no "$(succeeds fern restore "$env" p1)"
  fi
  for i in $(seq "$first" "$n"); do
    fern restore "$env" "p$i" >/dev/null
    if [ "$(sql "$env" "$whole" 2>/dev/null)" = t ] &&
      fern exec "$env" -- pg_amcheck --install-missing --heapallindexed postgres >/dev/null 2>&1; then
      ok=$((ok + 1))
    else
      note "snapshot p$i: Postgres isn't consistent"
    fi
  done
  metric pgload.snapshot_ms "snapshot under load, ms" "$(stats <"$TMP/pg-snap-$env")"
  check "Postgres is whole and uncorrupted after each of $((n - first + 1)) restores" $((n - first + 1)) "$ok"
}

pgload() {
  echo "== Postgres recovers from snapshots taken under load"
  fresh $ENV
  pg_load $ENV 60
}

idle() {
  echo "== an idle box goes to sleep"
  # The deployed arguments, such as --agent-boxes, stay.
  local args
  args=$(daemon_args)
  set_daemon_args $args --idle-agent=5s --idle-human=10s
  fresh $ENV
  fern exec $ENV -- true
  check "a box is awake after a command" running "$(state $ENV)"
  sleep 10
  check "and asleep 10 s after the command started" asleep "$(asleep_within $ENV 5)"
  fern exec $ENV -- sleep 20 &
  local open=$!
  sleep 15
  check "a running command keeps it awake past the idle times" running "$(state $ENV)"
  wait $open
  sleep 5
  check "it sleeps 5 s after the command ended" asleep "$(asleep_within $ENV 5)"
  fern wake $ENV >/dev/null
  restart_daemon
  sleep 10
  check "a box awake across a daemon restart goes to sleep too" asleep "$(asleep_within $ENV 5)"
  # Used at 0, the daemon restarting at 7: its clocks go on from 0, so it
  # sleeps by about 10, not at 17.
  fern exec $ENV -- true
  sleep 7
  restart_daemon
  check "a daemon restart doesn't start the idle clocks over" asleep "$(asleep_within $ENV 5)"
  set_daemon_args $args
}

upgrade() {
  echo "== commands run on through a daemon restart"
  fresh $ENV
  local out=$TMP/upgrade.out in=$TMP/upgrade.in code pid
  # A command that prints as it goes, for a while, and exits with a code of its own.
  fern exec $ENV -- sh -c 'echo $$ > /tmp/up.pid; for i in $(seq 1 40); do echo line $i; sleep 0.25; done; exit 7' >"$out" 2>&1 &
  pid=$!
  until [ -s "$out" ]; do sleep 0.1; done
  local before
  before=$(fern exec $ENV -- cat /tmp/up.pid)
  restart_daemon
  check "the command is the same process after the restart" "$before" "$(fern exec $ENV -- cat /tmp/up.pid)"
  check "and still running" yes "$(succeeds fern exec $ENV -- kill -0 "$before")"
  code=0
  wait $pid || code=$?
  check "its client gets its exit code" 7 "$code"
  check "and every line, once, in order" "$(seq 1 40 | sed 's/^/line /')" "$(cat "$out")"

  # stdin too: each side of the restart.
  : >"$out"
  rm -f "$in"
  mkfifo "$in"
  fern exec -i $ENV -- sh -c 'while read -r l; do echo "got $l"; done; echo bye' <"$in" >"$out" 2>&1 &
  pid=$!
  exec 9>"$in"
  echo one >&9
  until grep -q 'got one' "$out"; do sleep 0.1; done
  restart_daemon
  echo two >&9
  exec 9>&-
  wait $pid || true
  check "stdin goes on across the restart" "got one
got two
bye" "$(cat "$out")"

  # A Bash call, as Claude makes it, through the agent box's relay.
  if agent_boxes; then
    fern exec $ENV -- rm -f /tmp/bash.runs
    mcp $ENV Bash '{"command": "echo run >> /tmp/bash.runs; echo before; sleep 8; echo after; exit 4"}' >"$out" 2>&1 &
    pid=$!
    until [ "$(fern exec $ENV -- sh -c 'cat /tmp/bash.runs 2>/dev/null' | wc -l)" -ge 1 ]; do sleep 0.2; done
    restart_daemon
    wait $pid || true
    check "a Bash call gets its command's end across the restart" yes "$(succeeds grep -q after "$out")"
    check "and its exit code" yes "$(succeeds grep -q 'exit code 4' "$out")"
    check "and the command ran once" 1 "$(fern exec $ENV -- sh -c 'wc -l < /tmp/bash.runs' | tr -d ' ')"
    # Claude Code gives a tool's answer 60 s to begin, so the relay begins it
    # at once, as a stream, and sends the tool's answer when it is done.
    local call began
    call='{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "Bash", "arguments": {"command": "sleep 4"}}}'
    began=$(fern exec --agent $ENV -- curl -s -o /dev/null -w '%{time_starttransfer} %{time_total} %{content_type}' -m 60 \
      -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" -d "$call" "http://127.0.0.1:7072/mcp/$ENV")
    check "a tool call's answer begins while its command runs, as a stream" "yes yes text/event-stream" \
      "$(awk '{ print ($1 < 2 ? "yes" : "no"), ($2 >= 4 ? "yes" : "no"), $3 }' <<<"$began")"
    # Claude waits for a build with one call, not by looking again and again.
    local job waited
    job=$(mcp $ENV Bash '{"command": "sleep 3; echo built", "run_in_background": true}' | grep -o 'bash_[a-z0-9]*')
    waited=$(mcp $ENV BashOutput "{\"bash_id\": \"$job\", \"wait_seconds\": 60}")
    check "BashOutput waits for a background command to end, and has its output" "yes yes" \
      "$(grep -q 'exited with code 0' <<<"$waited" && echo yes || echo no) $(grep -q built <<<"$waited" && echo yes || echo no)"
  else
    note "skipped the Bash call: no agent boxes (lab/boxes.sh agents)"
  fi

  # A client that dies, with the daemon up, ends its command at once: no grace.
  on_node sh -c "exec $FERN_CLI exec $ENV -- sh -c 'echo \$\$ > /tmp/dies.pid; exec sleep 300'" </dev/null >/dev/null 2>&1 &
  pid=$!
  until fern exec $ENV -- test -s /tmp/dies.pid; do sleep 0.1; done
  local victim
  victim=$(fern exec $ENV -- cat /tmp/dies.pid)
  on_node pkill -KILL -f "dies.pid" </dev/null || true
  wait $pid 2>/dev/null || true
  sleep 2
  check "a client killed with the daemon up ends its command" no "$(succeeds fern exec $ENV -- kill -0 "$victim")"
}

# settings: what a repository can plant for Claude. A committed
# .claude/settings.json or .mcp.json would run code in the agent box, next to
# the login. A fake login means no model call and no subscription, and hooks
# and helpers run before any.
settings() {
  echo "== settings in a workspace don't run in the agent box"
  if ! agent_boxes; then
    note "skipped: no agent boxes (lab/boxes.sh agents)"
    return
  fi
  fresh $ENV
  # An agent is a program named claude: this one prints its arguments.
  # shellcheck disable=SC2016 # the box's shell's
  fern exec $ENV -- sh -c 'mkdir -p .fern/bin && printf "#!/bin/sh\nprintf \"%%s\\\\n\" \"\$@\"\n" >.fern/bin/claude && chmod 755 .fern/bin/claude'
  local given
  given=$(fern exec --agent $ENV -- ./.fern/bin/claude -p)
  check "Claude is pointed at the relay in its own box" yes "$(succeeds grep -q '127.0.0.1:7072/mcp/' <<<"$given")"
  check "… and is given no token: the relay adds the box's" no "$(succeeds grep -q 'Authorization' <<<"$given")"
  # Each box has a token of its own, made when it starts. The tool server
  # takes the agent box's and refuses the one of the box that runs the code.
  tool_server() {
    fern exec --agent $ENV -- curl -s -o /dev/null -w '%{http_code}' -m 10 -H "Authorization: Bearer $1" \
      -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
      -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}' \
      "http://fern-daemon:7071/mcp/$ENV"
  }
  # A token lives as long as its box: hold Claude's box awake to read it.
  fern exec --agent $ENV -- sleep 12 </dev/null >/dev/null 2>&1 &
  local hold=$! agent_token box_token
  for _ in $(seq 50); do
    in_daemon "test -s /run/fern/boxes/$ENV.agent/token" && break
    sleep 0.2
  done
  agent_token=$(in_daemon "cat /run/fern/boxes/$ENV.agent/token")
  box_token=$(in_daemon "cat /run/fern/boxes/$ENV/token")
  check "each box has a token of its own" yes "$([ -n "$agent_token" ] && [ -n "$box_token" ] && [ "$agent_token" != "$box_token" ] && echo yes || echo no)"
  check "the tool server takes the agent box's token" 200 "$(tool_server "$agent_token")"
  check "… and refuses the token of the box that runs the code" 401 "$(tool_server "$box_token")"
  check "no token is in the pool" "" "$(in_daemon "grep -rlF -e '${agent_token:-none}' -e '${box_token:-none}' /var/lib/fern/envs/$ENV 2>/dev/null")"
  # Its command over, Claude's box stops. The next start makes a new token,
  # and the old one opens nothing.
  wait $hold 2>/dev/null || true
  check "a token ends with its box" 401 "$(tool_server "$agent_token")"
  # shellcheck disable=SC2016 # the box's shell's
  fern exec $ENV -- sh -c 'mkdir -p .claude
    cat > .claude/m.sh <<"M"
echo ran >> "$PWD/marker-$1"
case "$1" in apiKeyHelper) echo fake-key;; esac
M
    cat > .claude/settings.json <<"J"
{"apiKeyHelper": "sh .claude/m.sh apiKeyHelper", "enableAllProjectMcpServers": true,
 "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "sh .claude/m.sh hook"}]}]}}
J
    cat > .mcp.json <<"J"
{"mcpServers": {"planted": {"type": "stdio", "command": "sh", "args": [".claude/m.sh", "mcpjson"]}}}
J'
  local args='--output-format stream-json --verbose --input-format stream-json --permission-mode bypassPermissions --allow-dangerously-skip-permissions'
  local turn='{"type":"control_request","request_id":"r1","request":{"subtype":"initialize","hooks":null}}
{"type":"user","session_id":"","message":{"role":"user","content":"say ok"},"parent_tool_use_id":null}'
  # The control: started through a shell, Fern adds no flags, and the planted
  # files do run. Without it this check could pass by planting nothing.
  # shellcheck disable=SC2086
  (printf '%s\n' "$turn"; sleep 12) | on_node env CLAUDE_CODE_OAUTH_TOKEN=fake-login $FERN_CLI exec -i -e CLAUDE_CODE_OAUTH_TOKEN --agent $ENV -- sh -c "exec claude $args" >/dev/null 2>&1 || true
  check "control: with no flags the planted files run" yes "$(succeeds fern exec $ENV -- sh -c 'ls marker-* >/dev/null 2>&1')"
  fern exec $ENV -- sh -c 'rm -f marker-*'
  # shellcheck disable=SC2086
  (printf '%s\n' "$turn"; sleep 12) | on_node env CLAUDE_CODE_OAUTH_TOKEN=fake-login $FERN_CLI exec -i -e CLAUDE_CODE_OAUTH_TOKEN --agent $ENV -- claude $args >"$TMP/settings.out" 2>&1 || true
  check "through Fern, none of them run" "" "$(fern exec $ENV -- sh -c 'ls marker-* 2>/dev/null')"
  check "and Claude started" yes "$(succeeds grep -q '"subtype":"init"' "$TMP/settings.out")"
  check "with Fern's MCP server" yes "$(succeeds grep -q '"name":"fern"' "$TMP/settings.out")"
  check "and not the planted one" no "$(succeeds grep -q '"name":"planted"' "$TMP/settings.out")"
}

daemon() {
  echo "== the daemon"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Grace', 'grace@example.com')" >/dev/null
  uid=$(pod_uid $ENV)
  start=$(now_ms)
  restart_daemon
  note "the daemon was back after $(($(now_ms) - start)) ms"
  check "the box kept running while the daemon restarted (same sandbox)" "$uid" "$(pod_uid $ENV)"
  check "Postgres kept its data (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
  check "the box still sees its workspace" yes "$(succeeds fern exec $ENV -- test -s seed.sql)"
  fern snapshot $ENV d1 >/dev/null
  fern restore $ENV d1 >/dev/null
  check "snapshot and restore work after the restart" 4 "$(sql $ENV 'select count(*) from users')"
  # Ctrl-C an rm while it waits for the box to stop: the daemon finishes it.
  in_daemon "timeout 1 fern rm $ENV" >/dev/null 2>&1 || true
  for _ in $(seq 60); do [ -n "$(state $ENV)" ] || break; sleep 1; done
  check "an rm whose client went away still finishes" "" "$(state $ENV)"
  check "and leaves no disk behind" no "$(succeeds in_daemon "test -e /var/lib/fern/envs/$ENV")"
}

node() {
  echo "== the node restarts"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Alan', 'alan@example.com')" >/dev/null
  fern snapshot $ENV n1 >/dev/null
  sql $ENV "insert into users (name, email) values ('Katherine', 'katherine@example.com')" >/dev/null
  start=$(now_ms)
  restart_node
  note "node and daemon back after $(($(now_ms) - start)) ms"
  start=$(now_ms)
  check "the box comes back with its data (5 users)" 5 "$(sql $ENV 'select count(*) from users')"
  note "first exec after the restart: $(($(now_ms) - start)) ms"
  fern restore $ENV n1 >/dev/null
  check "a snapshot from before the restart restores (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
}

many() {
  echo "== ten environments"
  for i in $(seq 10); do fresh "m$i"; done
  start=$(now_ms)
  for i in $(seq 10); do sql "m$i" 'select count(*) from users' >"$TMP/many$i" 2>&1 & done
  wait
  metric many.wake10_ms "10 sleeping environments woken at once, ms until all answered" $(($(now_ms) - start))
  check "all 10 answer (3 users each)" 10 "$(cat "$TMP"/many* | grep -cx 3 || true)"
  check "all 10 run under gVisor" 10 "$(box_uids m 10 | wc -w | tr -d ' ')"
  if [ -n "$FERN_HOST" ]; then
    metric many.node_memory10_mib "node memory used with 10 boxes awake, MiB" "$(on_node free -m </dev/null | awk 'NR == 2 { print $3 }')"
  else
    note "lab memory with 10 boxes awake: $(docker stats --no-stream --format '{{.MemUsage}}' "$LAB")"
  fi
  for i in $(seq 10); do fern rm "m$i"; done
}

# scale wakes more and more boxes and measures, at each step, what the step
# cost and how the node holds up with that many awake. Memory per box is
# the node's used memory above what it used before the first wake, divided
# by the boxes awake. Keep the legacy free-based metric for its baselines;
# node_used_per_box_mib uses the same definition on BusyBox and procps.
scale() {
  local steps n i prev=0 base used base_available used_available uids pod_stats box_mib kernel_mib parallel_ms
  local -a pod_uids
  read -ra steps <<<"${SCALE:-1 5 10 20}"
  echo "== scale: ${steps[*]} awake boxes"
  if [ "${SCALE_HARDWARE:-1}" = 1 ]; then
    hardware_uids
    hardware_sample >"$TMP/scale-before"
    metrics | awk '$1=="go_goroutines" {print "daemon_goroutines_count",$2}' >>"$TMP/scale-before"
  fi
  for i in $(seq "${steps[-1]}"); do fresh "s$i"; done
  base=$(on_node free -m </dev/null | awk 'NR == 2 { print $3 }')
  base_available=$(node_used)
  for n in "${steps[@]}"; do
    : >"$TMP/wakes$n"
    start=$(now_ms)
    for i in $(seq $((prev + 1)) "$n"); do fern wake "s$i" | took >>"$TMP/wakes$n" & done
    wait
    metric scale.n$n.all_awake_ms "boxes $((prev + 1))-$n woken at once, ms until all were ready" $(($(now_ms) - start))
    metric scale.n$n.wake_ms "their wakes, ms" "$(stats <"$TMP/wakes$n")"
    local answered=0
    for i in $(seq "$n"); do [ "$(sql "s$i" 'select count(*) from users' 2>/dev/null)" = 3 ] && answered=$((answered + 1)); done
    check "all $n awake boxes answer" "$n" "$answered"
    used=$(on_node free -m </dev/null | awk 'NR == 2 { print $3 }')
    metric scale.n$n.memory_per_box_mib "node memory per awake box, MiB" "$(awk -v u="$used" -v b="$base" -v n="$n" 'BEGIN { printf "%.1f", (u - b) / n }')"
    used_available=$(node_used)
    metric scale.n$n.node_used_per_box_mib "node memory per awake box, MiB (MemTotal - MemAvailable)" \
      "$(awk -v u="$used_available" -v b="$base_available" -v n="$n" 'BEGIN { printf "%.1f", (u - b) / n }')"
    # The box section measures a warm box. Read these fresh boxes' cgroups
    # here too, so their startup memory isn't mistaken for outside-pod cost.
    uids=$(box_uids s "$n")
    read -ra pod_uids <<<"$uids"
    check "memory accounts for all $n boxes" "$n" "${#pod_uids[@]}"
    if ! pod_stats=$(pod_cgroups memory.stat "${pod_uids[@]}"); then
      echo "FAIL reading memory of all $n awake boxes" >&2
      FAILED=1
      return
    fi
    read -r box_mib kernel_mib <<<"$(awk -v n="$n" '
      $1 == "anon" || $1 == "shmem" { box += $2 }
      $1 == "kernel" { kernel += $2 }
      END { printf "%.1f %.1f", box / 1048576 / n, kernel / 1048576 / n }' <<<"$pod_stats")"
    metric scale.n$n.box_memory_mib "box memory per awake box, MiB (anon + shmem)" "$box_mib"
    metric scale.n$n.pod_kernel_mib "kernel memory charged per awake pod, MiB" "$kernel_mib"
    metric scale.n$n.daemon_memory_mib "the daemon's memory, MiB" \
      "$(in_daemon 'cat /proc/1/status' | awk '$1 == "RssAnon:" { printf "%.1f", $2 / 1024 }')"
    if [ "${SCALE_HARDWARE:-1}" = 1 ]; then hardware_sample --counters "${pod_uids[@]}" >"$TMP/load-before$n"; fi
    : >"$TMP/exec$n"
    for _ in $(seq 20); do node_ms "$FERN_CLI" exec s1 -- true >>"$TMP/exec$n"; done
    metric scale.n$n.exec_ms "an exec in an awake box, ms" "$(stats <"$TMP/exec$n")"
    # shellcheck disable=SC2016 # expanded on the node
    parallel_ms=$(node_ms sh -c 'for i in $(seq "$1"); do "$2" exec "s$i" -- true & done; wait' sh "$n" "$FERN_CLI")
    metric scale.n$n.parallel_exec_ms "one exec in each awake box at once, ms until all ended" "$parallel_ms"
    # Collect individual latencies separately: timestamps and result files
    # must not inflate the existing parallel completion metric.
    if [ "${SCALE_HARDWARE:-1}" = 1 ]; then
      # shellcheck disable=SC2016 # expanded on the node
      on_node sh -c 'for i in $(seq "$1"); do
          (s=$(date +%s%N); "$2" exec "s$i" -- true; echo $((($(date +%s%N) - s) / 1000000))) &
        done; wait' sh "$n" "$FERN_CLI" </dev/null >"$TMP/parallel-$n"
      metric scale.n$n.contended_exec_ms "per-command latency with $n concurrent execs, ms" "$(stats <"$TMP/parallel-$n")"
    fi
    : >"$TMP/snap$n"
    for i in $(seq 20); do fern snapshot s1 "n$n-$i" | took >>"$TMP/snap$n"; done
    metric scale.n$n.snapshot_ms "a snapshot of an awake box, ms" "$(stats <"$TMP/snap$n")"
    if [ "${SCALE_HARDWARE:-1}" = 1 ]; then
      hardware_sample --counters "${pod_uids[@]}" >"$TMP/load-after$n"
      resource_rates scale.n$n.load "$TMP/load-before$n" "$TMP/load-after$n" "$n"
    fi
    prev=$n
  done
  for i in $(seq "${steps[-1]}"); do fern rm "s$i"; done
  if [ "${SCALE_HARDWARE:-1}" = 1 ]; then
    sleep 2
    hardware_sample >"$TMP/scale-after"
    metrics | awk '$1=="go_goroutines" {print "daemon_goroutines_count",$2}' >>"$TMP/scale-after"
    resource_stability scale "$TMP/scale-before" "$TMP/scale-after"
  fi
}

sizes() {
  echo "== the daemon's flags set a box's size"
  local args
  args=$(daemon_args)
  set_daemon_args $args --box-cpus=1.5 --box-memory=512 --box-pids=300
  fresh $ENV
  fern wake $ENV >/dev/null
  check "a box gets the CPUs asked for" "150000 100000" "$(pod_cgroup $ENV cpu.max)"
  check "and the memory" 536870912 "$(pod_cgroup $ENV memory.max)"
  check "and the processes" 300 "$(pod_cgroup $ENV pids.max)"
  if [ "$AGENTS" = 1 ]; then
    fern exec --agent $ENV -- sleep 6 &
    local open=$!
    sleep 3
    check "an agent box keeps its own size" 4096 "$(pod_cgroup $ENV.agent pids.max)"
    wait $open
  fi
  # A box's size is set when it wakes: one awake across the change keeps its own.
  fern wake $ENV >/dev/null
  set_daemon_args $args
  check "an awake box keeps its size when the flags change" 300 "$(pod_cgroup $ENV pids.max)"
  fern sleep $ENV >/dev/null
  fern wake $ENV >/dev/null
  check "and gets the new one at its next wake" 4096 "$(pod_cgroup $ENV pids.max)"
  # A repository asks for its own size in services.yaml, which its agent can
  # write: without --box-max-…, only for less.
  fern exec $ENV -- sh -c 'printf "limits:\n  memory: 512\n  pids: 100000\n" >>services.yaml'
  fern sleep $ENV >/dev/null
  fern wake $ENV >/dev/null
  check "a repository gets the memory it asks for, if less" 536870912 "$(pod_cgroup $ENV memory.max)"
  check "but no more processes than the node gives" 4096 "$(pod_cgroup $ENV pids.max)"
  set_daemon_args $args --box-max-pids=5000 --box-max-memory=4096
  fern exec $ENV -- sh -c 'sed -i "s/memory: 512/memory: 3072/" services.yaml'
  fern sleep $ENV >/dev/null
  fern wake $ENV >/dev/null
  check "with a higher most, it gets more memory" 3221225472 "$(pod_cgroup $ENV memory.max)"
  check "and processes up to the most" 5000 "$(pod_cgroup $ENV pids.max)"
  set_daemon_args $args
}

# opencode checks OpenCode's hibernation (fern exec --hibernate in front of
# `opencode serve`, as fern-opencode runs it): without a model, so it costs
# nothing.
opencode() {
  echo "== OpenCode's server stops when idle, and its agent box sleeps"
  if [ "$AGENTS" != 1 ]; then
    note "no agent boxes on this node: skipped"
    return
  fi
  fresh $ENV
  local out
  # shellcheck disable=SC2016 # the node's shell's
  out=$(on_node sh -s -- "$FERN_CLI" $ENV <<'NODE'
fern=$1 env=$2 port=4999 auth=opencode:pw t=$(mktemp -d)
box() { [ -d /sys/fs/cgroup/fern/$env.agent ] && echo awake || echo asleep; }
OPENCODE_PASSWORD=pw $fern exec --agent --hibernate 3s -e OPENCODE_PASSWORD $env -- \
  opencode serve --hostname=127.0.0.1 --port=$port >$t/out 2>$t/err &
fx=$!
for _ in $(seq 150); do grep -q listening $t/out && break; sleep 0.2; done
echo "printed=$(cat $t/out)"
curl -sN -u $auth http://127.0.0.1:$port/api/event >$t/events &
ce=$!
curl -s -o /dev/null -u $auth -X PUT -H 'Content-Type: application/json' \
  -d '{"config":{"type":"remote","url":"http://127.0.0.1:1/mcp","oauth":false}}' http://127.0.0.1:$port/api/experimental/mcp/probe
id=$(curl -s -u $auth -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:$port/api/session |
  python3 -c 'import json, sys; print(json.load(sys.stdin)["data"]["id"])')
echo "first=$(box)"
for _ in $(seq 60); do [ "$(box)" = asleep ] && break; sleep 0.2; done
echo "idle=$(box)"
echo "refused=$(curl -s -o /dev/null -w '%{http_code}' -u opencode:no http://127.0.0.1:$port/api/info) $(box)"
start=$(date +%s%N)
list=$(curl -s -u $auth http://127.0.0.1:$port/api/session)
echo "wake_ms=$((($(date +%s%N) - start) / 1000000))"
echo "kept=$(echo "$list" | grep -q "$id" && echo yes || echo no) $(box)"
echo "mcp=$(curl -s -u $auth http://127.0.0.1:$port/api/mcp | grep -q '"probe"' && echo yes || echo no)"
echo "stream=$(kill -0 $ce 2>/dev/null && echo open || echo closed) $(grep -c server.connected $t/events)"
kill -TERM $fx
wait $fx
kill $ce 2>/dev/null
rm -rf "$t"
NODE
)
  val() { sed -n "s/^$1=//p" <<<"$out"; }
  check "it prints its own URL, where T3 asked" "opencode server listening on http://127.0.0.1:4999" "$(val printed)"
  check "the agent box runs the server" awake "$(val first)"
  check "it sleeps once nothing is under way" asleep "$(val idle)"
  check "a request without the password doesn't wake it" "401 asleep" "$(val refused)"
  check "the next request wakes it, and the session is kept" "yes awake" "$(val kept)"
  check "T3's MCP server is added again" yes "$(val mcp)"
  check "T3's event stream stays open, with one server.connected" "open 1" "$(val stream)"
  note "the first request after a stop: $(val wake_ms) ms"
}

limits() {
  echo "== a box's limits"
  fresh $ENV
  fresh ${ENV}2
  # The runtime mounts the workspace without nosuid; no_new_privs is what
  # makes setuid files useless in the box.
  check "commands run with no_new_privs" 1 "$(fern exec $ENV -- grep NoNewPrivs /proc/self/status | awk '{print $2}')"
  check "the box has a CPU limit (2 cores)" "200000 100000" "$(pod_cgroup $ENV cpu.max)"
  check "the box has a PID limit" 4096 "$(pod_cgroup $ENV pids.max)"
  # A services.yaml fern-init can't read ends it as the box starts. The wake
  # says so from the box's log, and doesn't wait out its two minutes.
  fresh ${ENV}3
  fern exec ${ENV}3 -- sh -c 'echo "nonsense: 1" >>services.yaml'
  fern sleep ${ENV}3 >/dev/null
  local woke took
  took=$(date +%s)
  woke=$(fern wake ${ENV}3 2>&1 || true)
  took=$(($(date +%s) - took))
  check "a wake whose box exits at once fails with its log's end" yes "$(succeeds grep -q nonsense <<<"$woke")"
  check "… in seconds, not the wake's two minutes" yes "$([ "$took" -lt 20 ] && echo yes || echo "no: $took s")"
  # The box is gone and its folder with it, but not what it last printed;
  # and a box that keeps failing is no longer only the next command's error:
  # fern doctor lists it, and the thread's Claude is told.
  fern sleep ${ENV}3 >/dev/null 2>&1 || true
  check "a failed wake's log is kept, and fern logs prints it" yes \
    "$(fern logs ${ENV}3 2>&1 | grep -A20 "box ${ENV}3 isn't running: the log of its last run" | grep -q nonsense && echo yes || echo no)"
  for _ in 1 2 3; do fern exec ${ENV}3 -- true >/dev/null 2>&1 || true; done
  check "doctor lists a box that keeps crashing" 1 "$(fern doctor 2>/dev/null | grep -c "^FAIL  box  *${ENV}3 keeps crashing" || true)"
  if [ "${AGENTS:-0}" = 1 ]; then
    check "its thread's Claude is told" yes \
      "$(fern exec --agent ${ENV}3 -- cat /var/lib/fern/envs/${ENV}3/claude/CLAUDE.md | tr '\n' ' ' | grep -q "stopped starting this thread's box" && echo yes || echo no)"
  fi
  fern rm ${ENV}3
  fern wake ${ENV}2 >/dev/null
  ip=$(box_ip ${ENV}2)
  check "a box can't reach another box" no "$(succeeds fern exec $ENV -- curl -s -m 3 "http://$ip:7070/")"
  check "the daemon can" ok "$(fern exec ${ENV}2 -- echo ok)"
  fern rm ${ENV}2
  # The node's own address on the box's link. The daemon's metrics listen on
  # localhost unless --metrics-addr says otherwise; the boxes' firewall
  # keeps them out either way.
  ip=$(box_ip $ENV | awk -F. '{ printf "%s.%s.%s.%d", $1, $2, $3, $4 - 1 }')
  check "a box can't reach the daemon's metrics" no "$(succeeds fern exec $ENV -- curl -s -m 3 "http://$ip:9090/metrics")"
  check "or T3's server" no "$(succeeds fern exec $ENV -- curl -s -m 3 "http://$ip:30773/")"
  check "or the previews, which would reach every box" no "$(succeeds fern exec $ENV -- curl -s -m 3 "http://$ip:30780/")"
  if [ -n "$FERN_HOST" ]; then
    # The daemon replaces its own table (inet fern) whole at every start;
    # the VPS's firewall, which keeps T3's port private, has another.
    check "the VPS's firewall outlives the daemon's start" yes "$(succeeds on_node nft list table inet fern_host </dev/null)"
  fi
  check "but reaches the internet" yes "$(succeeds fern exec $ENV -- curl -s -m 10 -o /dev/null https://api.anthropic.com/)"
}

paths() {
  echo "== workspaces at a path, as T3's worktrees"
  local W=/var/lib/fern/worktrees/fern-check
  for e in at-path adopted adopted-big; do fern rm $e 2>/dev/null || true; done
  fern create at-path --path $W/main >/dev/null
  check "the box works in the workspace's own path" $W/main "$(fern exec at-path -- pwd)"
  tar_dir lab/sample-repo | fern exec -i at-path -- tar -x -f -
  fern sleep at-path >/dev/null
  check "a workspace at a path runs Postgres (3 users)" 3 "$(sql at-path 'select count(*) from users')"
  check "which finds it from a folder inside" at-path "$(fern which $W/main/scripts)"
  # What fern-claude does for T3: the environment and directory from the cwd.
  check "exec --here runs in the current directory" $W/main/scripts \
    "$(in_daemon "cd $W/main/scripts && fern exec --here -- pwd")"
  check "exec -e passes a variable" sdk-ts \
    "$(in_daemon "cd $W/main && CLAUDE_CODE_ENTRYPOINT=sdk-ts fern exec --here -e CLAUDE_CODE_ENTRYPOINT -- sh -c 'echo \$CLAUDE_CODE_ENTRYPOINT'")"
  # A worktree T3 made before Fern saw it is adopted.
  tar_dir lab/sample-repo | in_daemon "mkdir -p $W/adopted && tar -x -C $W/adopted -f - && chown -R 1000:1000 $W/adopted"
  metric paths.adopt_ms "adopting the sample repo, ms" "$(node_ms "$FERN_CLI" create adopted --path $W/adopted)"
  check "the adopted workspace is a subvolume" 256 "$(in_daemon "stat -c %i $W/adopted")"
  check "the adopted workspace runs Postgres (3 users)" 3 "$(sql adopted 'select count(*) from users')"
  in_daemon "mkdir -p $W/big && cd $W/big && seq 20000 | xargs -n 2000 sh -c 'for i; do echo \$i > f\$i; done' _ && chown -R 1000:1000 $W/big"
  metric paths.adopt_20k_ms "adopting 20,000 files, ms" "$(node_ms "$FERN_CLI" create adopted-big --path $W/big)"
  check "all 20,000 files adopted" 20000 "$(in_daemon "ls $W/big | wc -l" | tr -d ' ')"
  # T3 reuses a checkpoint's name after a revert, so the snapshot is taken again.
  fern snapshot adopted c1 >/dev/null
  sql adopted 'delete from users' >/dev/null
  fern snapshot --replace adopted c1 >/dev/null
  sql adopted "insert into users (name, email) values ('Grace', 'grace@example.com')" >/dev/null
  fern restore adopted c1 >/dev/null
  check "a replaced snapshot holds the new state (0 users)" 0 "$(sql adopted 'select count(*) from users')"
  fern rmsnap adopted c1
  check "rmsnap removes it" 0 "$(fern snapshots adopted | grep -c '^c1 ' || true)"
  for e in at-path adopted adopted-big; do fern rm $e; done
  check "rm removes the workspaces at their paths" "" "$(in_daemon "ls $W")"
  # The adopted sample started a golden build for its repo.
  fern rm "$(golden_id fern-check)" 2>/dev/null || true
}

golden() {
  echo "== golden snapshots"
  local W=/var/lib/fern/worktrees/golden-check G key
  G=$(golden_id golden-check)
  for e in gold1 gold2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  for wt in one two; do
    tar_dir lab/sample-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - && chown -R 1000:1000 $W/$wt"
  done
  # The first new worktree of a repository has no golden yet: it starts as
  # it is, and its checkout is built into one in the background.
  fern create gold1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 120); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric golden.build_ms "golden build, ms" $(($(now_ms) - start))
  check "a golden snapshot is built from the first worktree" 1 "$(grep -c . <<<"$key")"
  check "it holds the seeded database, not the checkout" "yes no" \
    "$(in_daemon "cd /var/lib/fern/envs/$G/snaps/$key && { test -e .fern/seeded && echo yes || echo no; } && { test -e seed.sql && echo yes || echo no; }" | xargs)"
  check "the first worktree's source copy is gone" 0 "$(fern snapshots gold1 | grep -c golden-source || true)"
  # The next one starts from it: its database is there before its first wake.
  metric golden.create_ms "create from golden, ms" "$(node_ms "$FERN_CLI" create gold2 --path $W/two)"
  check "the next worktree has the database before its first wake" yes \
    "$(in_daemon "test -e $W/two/.fern/seeded && test -e $W/two/seed.sql" && echo yes || echo no)"
  pool_du golden.snapshot "/var/lib/fern/envs/$G/snaps/$key"
  metric golden.first_wake_ms "first wake from golden, ms" "$(resource_op golden.first_wake wake gold2 | took)"
  metric golden.first_wake_cold_ms "first wake without one (initdb and seed), ms" "$(resource_op golden.first_wake_cold wake gold1 | took)"
  check "and Postgres runs on it (3 users)" 3 "$(sql gold2 'select count(*) from users')"
  for e in gold1 gold2 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"

  # T3 sends a project's checkout when it is added, so that even its first
  # worktree starts from a golden.
  W=/var/lib/fern/worktrees/golden-pre
  G=$(golden_id golden-pre)
  for e in gold3 "$G"; do fern rm "$e" 2>/dev/null || true; done
  tar_dir lab/sample-repo | on_node -i "$FERN_CLI" prebuild golden-pre
  start=$(now_ms)
  key=
  for _ in $(seq 120); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric golden.prebuild_ms "golden build from a project's checkout, ms" $(($(now_ms) - start))
  check "a project's checkout is built into a golden" 1 "$(grep -c . <<<"$key")"
  check "its source environment is gone" no "$(succeeds in_daemon "test -e /var/lib/fern/envs/$G-src")"
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/one && mkdir -p $W/one && tar -x -C $W/one -f - && chown -R 1000:1000 $W/one"
  fern create gold3 --path $W/one >/dev/null
  check "the project's first worktree has the database before its first wake" yes \
    "$(in_daemon "test -e $W/one/.fern/seeded" && echo yes || echo no)"
  for e in gold3 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"

  # A repository's setup script runs in the build, before its services'
  # first start, and what it makes is in the golden.
  W=/var/lib/fern/worktrees/golden-setup
  G=$(golden_id golden-setup)
  for e in gold4 gold5 "$G"; do fern rm "$e" 2>/dev/null || true; done
  for wt in one two; do
    tar_dir lab/sample-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt/.fern && tar -x -C $W/$wt -f - &&
      printf '%s\n' '#!/bin/sh' 'set -eu' 'mkdir -p .fern/built' \
        'if test -e .fern/seeded; then echo after; else echo before; fi > .fern/built/services' \
        'echo by-setup > .fern/built/artifact' > $W/$wt/.fern/setup &&
      chmod +x $W/$wt/.fern/setup && chown -R 1000:1000 $W/$wt"
  done
  fern create gold4 --path $W/one >/dev/null
  key=
  for _ in $(seq 120); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  check "a golden is built from a repository with a setup script" 1 "$(grep -c . <<<"$key")"
  fern create gold5 --path $W/two >/dev/null
  check "the next worktree has what the script made, and the seeded database" "by-setup yes" \
    "$(in_daemon "cat $W/two/.fern/built/artifact; test -e $W/two/.fern/seeded && echo yes || echo no" | xargs)"
  check "the script ran before the services' first start" before "$(in_daemon "cat $W/two/.fern/built/services")"
  check "and the worktree's own script is its checkout's" yes "$(succeeds in_daemon "test -x $W/two/.fern/setup")"
  check "Postgres runs on it (3 users)" 3 "$(sql gold5 'select count(*) from users')"
  for e in gold4 gold5 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"

  # A setup script that fails leaves no golden, and it shows where a person
  # looks: fern doctor lists it, a metric counts it, and the thread's Claude
  # is told, to pass it on. Until 2026-11-13 it was a line in the daemon's
  # log. A build that works clears it.
  W=/var/lib/fern/worktrees/golden-fails
  G=$(golden_id golden-fails)
  for e in gold6 gold7 "$G"; do fern rm "$e" 2>/dev/null || true; done
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/one && mkdir -p $W/one/.fern && tar -x -C $W/one -f - &&
    printf '%s\n' '#!/bin/sh' 'echo no such tool: frobnicate >&2' 'exit 3' > $W/one/.fern/setup &&
    chmod +x $W/one/.fern/setup && chown -R 1000:1000 $W/one"
  fern create gold6 --path $W/one >/dev/null
  for _ in $(seq 120); do
    in_daemon "test -e /var/lib/fern/envs/$G/build-failed.json" 2>/dev/null && break
    sleep 1
  done
  check "a setup script that fails is recorded with its builder, and no golden is made" "yes 0" \
    "$(succeeds in_daemon "grep -q frobnicate /var/lib/fern/envs/$G/build-failed.json") $(fern snapshots "$G" 2>/dev/null | grep -c '^golden-' || true)"
  check "fern doctor lists the failed build, with what the script printed" 1 \
    "$(fern doctor 2>/dev/null | grep -c "^FAIL  golden build .*$G.*frobnicate" || true)"
  check "the daemon's metrics count it" yes \
    "$(succeeds in_daemon "curl -sf http://127.0.0.1:9090/metrics | grep -q '^fern_problems [1-9]'")"
  hint="fern exec gold6"
  agent_boxes && hint="fern exec --agent gold6"
  check "the thread's Claude is told, and to tell the person" yes \
    "$($hint -- cat /var/lib/fern/envs/gold6/claude/CLAUDE.md | tr '\n' ' ' | grep -q 'Tell the person.*starting point failed.*frobnicate' && echo yes || echo no)"
  # A box's own log lasts while it runs: the services' box is up after a command.
  fern exec gold6 -- true
  check "fern logs has the daemon's lines about the thread, and its running box's own log" "yes yes" \
    "$(out=$(fern logs gold6 2>&1); { grep -q 'msg="box woke" env=gold6' <<<"$out" && echo yes || echo no; grep -q "== box gold6's own log" <<<"$out" && echo yes || echo no; } | xargs)"
  in_daemon "printf '%s\n' '#!/bin/sh' 'mkdir -p .fern/built && echo mended > .fern/built/artifact' > $W/one/.fern/setup"
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/two && mkdir -p $W/two/.fern && tar -x -C $W/two -f - &&
    cp -p $W/one/.fern/setup $W/two/.fern/setup && chown -R 1000:1000 $W/two"
  fern create gold7 --path $W/two >/dev/null
  key=
  for _ in $(seq 120); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  check "the mended script builds a golden, and the failure is gone from disk and from doctor" "1 no 0" \
    "$(grep -c . <<<"$key") $(succeeds in_daemon "test -e /var/lib/fern/envs/$G/build-failed.json") $(fern doctor 2>/dev/null | grep -c 'golden build' || true)"
  for e in gold6 gold7 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}

claude() {
  echo "== Claude Code in a box"
  fresh $ENV
  check "claude runs" "$CLAUDE" "$(fern exec $ENV -- claude --version)"
  check "the box reaches api.anthropic.com" yes "$(succeeds fern exec $ENV -- curl -s -o /dev/null https://api.anthropic.com/)"
  # Without the login, so it costs nothing: the answer is an error, in stream-json.
  out=$(printf '%s\n' '{"type":"user","message":{"role":"user","content":"say hi"}}' |
    fern exec -i $ENV -- env -u CLAUDE_CODE_OAUTH_TOKEN \
      claude -p --input-format stream-json --output-format stream-json --verbose 2>/dev/null || true)
  check "stream-json arrives as whole JSON lines, the result last" result "$(python3 -c '
import json, sys
try:
    print([json.loads(l) for l in sys.stdin if l.strip()][-1]["type"])
except Exception as e:
    print("broken:", e)' <<<"$out")"
  # Through a terminal, as "fern exec -it" gives it. At this first-run screen
  # claude doesn't quit on two Ctrl-Cs, with or without Fern, so a timeout
  # ends it.
  tui=$(in_daemon "sleep 9 | timeout 8 script -qec 'fern exec -it $ENV -- claude' /dev/null" 2>/dev/null || true)
  # It draws the spaces between words as cursor moves.
  check "claude draws its screen through a terminal" yes "$(succeeds grep -qa Welcome <<<"$tui")"
}

# mcp ENV TOOL ARGS calls TOOL (Bash, BashOutput, KillShell, env_fork, …) of environment
# ENV's MCP server with ARGS, a JSON object, from its agent box, as Claude
# there does: through the box's relay, which adds the box's token (Claude is
# given none), and prints the answer's text; it fails if the tool did. The
# relay answers a tool call as a stream, whose one message is the answer.
mcp() {
  local call out
  call=$(python3 -c 'import json, sys
print(json.dumps({"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": sys.argv[1], "arguments": json.loads(sys.argv[2])}}))' "$2" "$3")
  # shellcheck disable=SC2016 # the box's shell's
  out=$(fern exec --agent "$1" -- sh -c '
    for body in "{\"jsonrpc\": \"2.0\", \"id\": 1, \"method\": \"initialize\", \"params\": {\"protocolVersion\": \"2025-06-18\", \"capabilities\": {}, \"clientInfo\": {\"name\": \"check\", \"version\": \"1\"}}}" "$2"; do
      out=$(curl -sf -m 600 -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" -d "$body" "http://127.0.0.1:7072/mcp/$1") || exit
    done
    printf %s "$out"' sh "$1" "$call") || return
  python3 -c 'import json, sys
data = [line[6:] for line in sys.stdin.read().splitlines() if line.startswith("data: ")]
res = json.loads(data[-1])["result"]
print(res["content"][0]["text"])
sys.exit(1 if res.get("isError") else 0)' <<<"$out"
}

# commit checks Claude's commit tool on a worktree as T3 makes them: the box
# has the repository's git folder read-only, so the daemon commits on the
# node, as T3's user and with the repository's hooks off.
commit() {
  echo "== Claude commits through the daemon"
  if [ "${AGENTS:-0}" != 1 ]; then
    note "skipped: no agent boxes (lab/boxes.sh agents)"
    return
  fi
  local R=/var/lib/fern/repos/ck-commit W=/var/lib/fern/worktrees/ck-commit/one out
  local as="setpriv --reuid 1000 --regid 1000 --clear-groups env HOME=/var/lib/fern/projects"
  fern rm $ENV 2>/dev/null || true
  in_daemon "rm -rf $R /var/lib/fern/worktrees/ck-commit && install -d -o 1000 -g 1000 $R /var/lib/fern/worktrees/ck-commit /var/lib/fern/projects &&
    cd $R && $as git init -q -b main . && $as git config user.name Check && $as git config user.email check@fern &&
    $as sh -c 'echo v0 > a.txt' && $as git add a.txt && $as git commit -qm init && $as git worktree add -q -b t3/one $W &&
    $as sh -c 'printf \"#!/bin/sh\\ntouch $R/hook-ran\\n\" > .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit'"
  fern create $ENV --path $W >/dev/null
  fern exec $ENV -- sh -c 'echo v1 > a.txt && echo new > b.txt'
  # A repository that asks for a bigger box than the node gives: Claude is
  # told, in its hint, at its box's next start.
  fern exec $ENV -- sh -c 'printf "limits:\n  cpus: 64\n" > services.yaml'
  fern sleep $ENV >/dev/null
  check "Claude is told its box has fewer CPUs than the repository asks for" yes \
    "$(fern exec --agent $ENV -- cat /var/lib/fern/envs/$ENV/claude/CLAUDE.md | grep -q '64 CPUs (the box has' && echo yes || echo no)"
  fern exec $ENV -- rm services.yaml
  check "git in the box can't commit: its git folder is read-only" no \
    "$(succeeds fern exec $ENV -- git -c user.name=x -c user.email=x@x commit -qam x)"
  out=$(mcp $ENV commit '{"message": "from the thread\n\nWith a body."}')
  check "the commit tool says what it committed" yes "$(succeeds grep -q '^Committed 2 files to t3/one as ' <<<"$out")"
  check "the commit is on the thread's branch, by the repository's author" "from the thread Check <check@fern>" \
    "$(in_daemon "cd $R && $as git log -1 --format='%s %an <%ae>' t3/one")"
  check "main is untouched" init "$(in_daemon "cd $R && $as git log -1 --format=%s main")"
  check "the repository's hook didn't run" no "$(succeeds in_daemon "test -e $R/hook-ran")"
  check "the git folder is still all T3's user's" 0 "$(in_daemon "find $R/.git ! -user 1000 | wc -l" | tr -d ' ')"
  check "the box sees the commit" "from the thread" "$(fern exec $ENV -- git log -1 --format=%s)"
  check "and a worktree with nothing left to commit" "" "$(fern exec $ENV -- git status --short)"
  check "which the tool says too" "Nothing to commit: the worktree has no changes." "$(mcp $ENV commit '{"message": "again"}')"
  # A repository the box makes in a folder has a config of the box's own.
  # Staged as a submodule, git add and git status on the node would start a
  # git in it, which runs the filter that config names: the box's program,
  # outside the box (found 2026-11-13). The tool commits around it, twice,
  # runs nothing, and leaves no submodule in the index for T3's git to find.
  fern exec $ENV -- sh -c 'git init -q vendor/x && cd vendor/x && echo f > f && git add f &&
    git -c user.name=x -c user.email=x@x commit -qm theirs &&
    git config filter.mark.clean "sh -c \"echo ran >> /var/lib/fern/worktrees/ck-commit/one/filter-ran; cat\"" &&
    echo "* filter=mark" > .git/info/attributes && echo changed > f &&
    cd ../.. && printf "[submodule \"vendor/x\"]\n\tpath = vendor/x\n\turl = ./vendor/x\n\tignore = none\n" > .gitmodules && echo more >> a.txt'
  out=$(mcp $ENV commit '{"message": "around a nested repository"}')
  fern exec $ENV -- sh -c 'echo again >> a.txt; echo changed again > vendor/x/f'
  out="$out $(mcp $ENV commit '{"message": "and once more"}')"
  check "a repository the box made in a folder is left out, and the tool says so" 2 "$(grep -o 'Left out, because each holds a git repository of its own: vendor/x/' <<<"$out" | wc -l | tr -d ' ')"
  check "its filter didn't run on the node, and the index holds no submodule" "no 0" \
    "$(succeeds in_daemon "test -e $W/filter-ran") $(in_daemon "cd $W && $as git ls-files --stage | grep -c ^160000" || true)"
}

# devserver starts Vite in the environment's box as Claude does, with MCP
# Bash's run_in_background, and checks that it keeps running after the call
# (a Bash call alone lets the box sleep 5 s later), then changes files from
# the agent box, as Claude's Edit and Write do: gVisor tells a box only of its
# own changes, so Fern passes them on (package watch). Vite is reached from
# the node, at the box's address: a command in the box would start the box's
# idle clocks and keep it awake by itself.
devserver() {
  echo "== a dev server Claude starts keeps running and hears Claude's edits"
  if [ "${AGENTS:-0}" != 1 ]; then
    note "skipped: no agent boxes (lab/boxes.sh agents)"
    return
  fi
  fern rm $ENV 2>/dev/null || true
  fern create $ENV >/dev/null
  # The box image has neither npm nor pnpm: the repository pins them, and
  # the box installs them when it wakes.
  fern exec $ENV -- sh -c 'printf "[tools]\nnode = \"22\"\npnpm = \"10\"\n" > mise.toml'
  fern sleep $ENV >/dev/null
  start=$(now_ms)
  check "a wake installs the tools mise.toml pins" v22 "$(fern exec $ENV -- node --version | cut -d. -f1)"
  note "a wake that installs Node 22 and pnpm 10, ms: $(($(now_ms) - start))"
  fern exec $ENV -- sh -c '
    set -e
    mkdir -p src
    echo "{}" > package.json
    pnpm add vite@7 >/dev/null 2>&1
    echo "<script type=module src=/src/main.js></script>" > index.html
    echo "export const v = 1" > src/main.js
    cat > vite.config.mjs <<EOF
import { appendFileSync } from "node:fs";
import { relative } from "node:path";
// Every event of the watcher Vite uses, in /tmp, where it is not watching.
export default {
  plugins: [{ name: "log", configureServer(s) {
    s.watcher.on("all", (e, p) => appendFileSync("/tmp/watch.log", e + " " + relative(process.cwd(), p) + "\\n"));
  } }],
};
EOF'
  # From here on only Claude's tools wake and use the box.
  fern sleep $ENV >/dev/null
  local job
  job=$(mcp $ENV Bash '{"command": "pnpm exec vite --host 0.0.0.0 --port 5173 --strictPort", "run_in_background": true}' |
    grep -o 'bash_[a-z0-9]*')
  check "Bash starts Vite in the background" yes "$([ -n "$job" ] && echo yes || echo no)"
  local ip
  ip=$(box_ip $ENV)
  # served PATTERN waits up to 10 s for Vite to serve src/main.js matching it.
  served() {
    # shellcheck disable=SC2016 # expanded on the node
    on_node sh -c 'for i in $(seq 100); do curl -sf "http://$1:5173/src/main.js" | grep -q "$2" && exit; sleep 0.1; done; exit 1' sh "$ip" "$1" </dev/null
  }
  if ! served 'v = 1' >/dev/null 2>&1; then
    check "Vite serves src/main.js" yes no
    mcp $ENV BashOutput "{\"bash_id\": \"$job\"}" | sed 's/^/  vite: /'
    return
  fi
  sleep 65
  check "a minute after the Bash call, the box is awake" running "$(state $ENV)"
  check "… and Vite still serves" yes "$(succeeds served 'v = 1')"
  check "BashOutput shows Vite's output" yes "$(succeeds grep -q 'ready in' <<<"$(mcp $ENV BashOutput "{\"bash_id\": \"$job\"}")")"
  restart_daemon
  check "after the daemon restarts, Vite still serves" yes "$(succeeds served 'v = 1')"
  check "… and BashOutput still reaches it" yes "$(succeeds grep -q 'is running' <<<"$(mcp $ENV BashOutput "{\"bash_id\": \"$job\"}")")"

  start=$(now_ms)
  fern exec --agent $ENV -- sh -c '
    echo "export const v = 2" > src/main.js
    echo "export const n = 1" > src/new.js
    mkdir -p src/feature/deep && echo "export const d = 1" > src/feature/deep/d.js'
  check "after Claude's edit, Vite serves the new src/main.js" yes "$(succeeds served 'v = 2')"
  note "edit to served: $(($(now_ms) - start)) ms, the agent box's exec included"
  # heard EVENT waits up to 5 s for Vite's watcher to report it.
  heard() {
    local out
    out=$(mcp $ENV Bash "{\"command\": \"for i in \$(seq 50); do if grep -Eqx '$1' /tmp/watch.log; then exit 0; fi; sleep 0.1; done; exit 1\"}") || return
    ! grep -q 'exit code' <<<"$out"
  }
  check "Vite's watcher hears the edit" yes "$(succeeds heard 'change src/main.js')"
  check "… a new file" yes "$(succeeds heard '(add|change) src/new.js')"
  check "… a file in new folders" yes "$(succeeds heard '(add|change) src/feature/deep/d.js')"
  note "Vite's watcher heard: $(mcp $ENV Bash '{"command": "sort -u /tmp/watch.log | tr \"\\n\" \";\""}')"

  check "KillShell stops Vite" yes "$(succeeds grep -q 'is killed' <<<"$(mcp $ENV KillShell "{\"shell_id\": \"$job\"}")")"
  check "… and then the box sleeps after the last Bash call's 5 s" asleep "$(asleep_within $ENV 20)"

  # Previews, as a browser on the machine asks for them. A preview request
  # wakes the box and counts as its use, so these come last.
  # preview HOST PATH prints what the front door answers for HOST.
  preview() {
    on_node curl -s -H "Host: $1" "http://127.0.0.1:30780$2" </dev/null
  }
  check "a preview of a port nothing listens on says so" yes "$(succeeds grep -q 'nothing listens on port 5173' <<<"$(preview "5173-$ENV.localhost:30780" /)")"
  # A dev server in services.yaml starts with the box, which a preview wakes.
  fern exec $ENV -- sh -c 'printf "services:\n  web:\n    run: pnpm exec vite --host 0.0.0.0 --port \$PORT --strictPort\n    port: 5173\n" > services.yaml'
  fern sleep $ENV >/dev/null
  start=$(now_ms)
  check "the default preview http://$ENV.localhost:30780/ wakes the box and serves Vite" yes "$(succeeds grep -q 'v = 2' <<<"$(preview "$ENV.localhost:30780" /src/main.js)")"
  note "asleep to served through the preview: $(($(now_ms) - start)) ms"
  check "… as does http://5173-$ENV.localhost:30780/" yes "$(succeeds grep -q 'v = 2' <<<"$(preview "5173-$ENV.localhost:30780" /src/main.js)")"
  check "… with its live reload's client" yes "$(succeeds grep -q WebSocket <<<"$(preview "$ENV.localhost:30780" /@vite/client)")"
  fern exec --agent $ENV -- sh -c 'echo "export const v = 3" > src/main.js'
  check "an edit of Claude's reaches the preview" yes "$(succeeds on_node sh -c 'for i in $(seq 100); do curl -s -H "Host: $1" http://127.0.0.1:30780/src/main.js | grep -q "v = 3" && exit; sleep 0.1; done; exit 1' sh "$ENV.localhost:30780" </dev/null)"
}

# tools builds a golden snapshot of a TanStack Start repository that pins
# Node 22 and pnpm 10 (lab/tanstack-repo), which the box image hasn't got,
# and serves a thread of it through a preview.
tools() {
  echo "== a repository's pinned tools, in its golden snapshot"
  local W=/var/lib/fern/worktrees/tools-check G key
  G=$(golden_id tools-check)
  for e in tools1 tools2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  fresh $ENV
  check "the box image has no npm or pnpm" "no no" \
    "$(fern exec $ENV -- sh -c 'for t in npm pnpm; do PATH=/usr/local/bin:/usr/bin:/bin command -v $t >/dev/null && echo yes || echo no; done' | xargs)"
  for wt in one two; do
    tar_dir lab/tanstack-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - && chown -R 1000:1000 $W/$wt"
  done
  fern create tools1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 300); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric tools.golden_build_ms "golden build with Node, pnpm and the dependencies, ms" $(($(now_ms) - start))
  check "a golden snapshot is built" 1 "$(grep -c . <<<"$key")"
  if [ -z "$key" ]; then
    box_log "$G" | tail -20 | sed 's/^/  builder: /'
    return
  fi
  check "it holds Node 22, pnpm and node_modules, not the checkout" "yes yes yes no" \
    "$(in_daemon "cd /var/lib/fern/envs/$G/snaps/$key && for p in .fern/tools/installs/node/22 .fern/tools/installs/pnpm node_modules/.pnpm package.json; do test -e \$p && echo yes || echo no; done" | xargs)"
  pool_du tools.golden "/var/lib/fern/envs/$G/snaps/$key"
  metric tools.create_ms "create from golden, ms" "$(node_ms "$FERN_CLI" create tools2 --path $W/two)"
  check "git ignores the tools" '*' "$(in_daemon "cat $W/two/.fern/tools/.gitignore")"
  # preview HOST PATH prints what the front door answers for HOST.
  preview() {
    on_node curl -s -H "Host: $1" "http://127.0.0.1:30780$2" </dev/null
  }
  start=$(now_ms)
  check "a preview wakes the thread and TanStack Start serves its page" yes \
    "$(succeeds grep -q 'Hello world' <<<"$(preview "tools2.localhost:30780" /)")"
  note "asleep to TanStack Start's page through the preview, from golden: $(($(now_ms) - start)) ms"
  # A command wakes the box, as Claude's turn does: the daemon renders the
  # default preview ahead, so the person who opens it then finds it done.
  fern sleep tools2 >/dev/null
  fern exec tools2 -- true >/dev/null
  sleep 30
  start=$(now_ms)
  check "after a command's wake, the page is rendered ahead" yes \
    "$(succeeds grep -q 'Hello world' <<<"$(preview "tools2.localhost:30780" /)")"
  local ahead=$(($(now_ms) - start))
  metric tools.page_after_wake_ms "the page through the preview 30 s after a command woke the box, ms" "$ahead"
  check "… and served at once (under 3 s)" yes "$([ "$ahead" -lt 3000 ] && echo yes || echo no)"
  check "Claude's Bash gets the pinned Node and pnpm" "v22 10" \
    "$(fern exec tools2 -- sh -c 'echo $(node --version | cut -d. -f1) $(pnpm --version | cut -d. -f1)')"
  for e in tools1 tools2 "$G" $ENV; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}

t3() {
  echo "== T3's server on Fern's node"
  if ! t3_installed || ! wait_t3 2>/dev/null; then
    check "T3's server runs (lab/build.sh t3)" yes no
    return
  fi
  # A VPS's T3 is reached through an SSH tunnel at T3_PORT.
  check "the Mac reaches T3" 200 \
    "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${T3_PORT:-30773}/.well-known/t3/environment")"
  check "T3 runs as uid 1000" 1000 "$(in_t3 'id -u')"
  check "T3 reaches the daemon's socket" yes "$(succeeds in_t3 'fern ls')"
  check "claude outside a workspace is the local one" "$CLAUDE" "$(in_t3 'cd / && claude --version')"
  check "T3 releases idle provider sessions after 5 minutes (fern-sessions.patch)" 1 \
    "$(in_t3 'grep -l T3CODE_PROVIDER_IDLE_TIMEOUT_MS /opt/t3/apps/server/dist/*.mjs | wc -l' || true)"
  # What the checkpoint hook (images/t3/fern-checkpoints.patch) does with a
  # worktree T3 made: adopt it at the first checkpoint, then snapshot,
  # replace and restore.
  local R=/var/lib/fern/repos/t3check W=/var/lib/fern/worktrees/t3check/one
  fern rm t3c 2>/dev/null || true
  in_t3 "rm -rf $R /var/lib/fern/worktrees/t3check && git init -q -b main $R && cd $R && echo v0 > a.txt \
    && git add a.txt && git -c user.email=t@fern -c user.name=t commit -qm init && git worktree add -q -b t3/one $W"
  metric t3.adopt_ms "T3's worktree adopted (first checkpoint), ms" \
    "$(in_t3 "s=\$(date +%s%N); fern create --path $W t3c >/dev/null && echo \$(((\$(date +%s%N) - s) / 1000000))")"
  check "claude in a T3 worktree runs" "$CLAUDE" "$(in_t3 "cd $W && claude --version")"
  # Claude's box: the environment's own, or with agent boxes its agent box.
  local x="--here"
  if [ "${AGENTS:-0}" = 1 ]; then
    x="--here --agent"
    # The agent box sleeps as soon as Claude ends: the sign is the other.
    check "in its agent box: the box with the services stays asleep" asleep "$(state t3c)"
    check "the box with the services has no Claude login or folder" "0 no" \
      "$(in_t3 "cd $W && fern exec --here -- sh -c 'env | grep -c ^CLAUDE_; test -d /var/lib/fern/envs/t3c/claude && echo yes || echo no'" | xargs)"
  else
    check "in its box, which it woke" running "$(state t3c)"
  fi
  # Claude's folder is outside the workspace, so sleep and restore keep it.
  local C=/var/lib/fern/envs/t3c/claude
  check "the box keeps Claude's state outside the workspace" "$C" \
    "$(in_t3 "cd $W && fern exec $x -- sh -c 'echo \$CLAUDE_CONFIG_DIR'")"
  in_t3 "cd $W && fern exec $x -- sh -c 'echo kept > $C/note'"
  # T3 runs as the user that owns Claude's folder, and a line planted there
  # would run in the thread's agent box: its service has the environments
  # read-only (deploy/systemd/fern-t3.service).
  check "T3's service reads an environment's Claude folder, and can't write it" "kept no" \
    "$(in_t3 "cat $C/note" | xargs) $(succeeds in_t3 "touch $C/planted")"
  check "Claude is told it is in a Fern environment" yes \
    "$(in_t3 "cd $W && fern exec $x -- cat $C/CLAUDE.md" | grep -q 'Fern environment' && echo yes || echo no)"
  check "git works in the box" init "$(in_t3 "cd $W && fern exec --here -- git log --format=%s -1")"
  check "and can't change the main repository" no "$(succeeds in_t3 "cd $W && fern exec --here -- touch $R/.git/x")"
  check "or point the worktree's .git elsewhere" no \
    "$(succeeds in_t3 "cd $W && fern exec --here -- sh -c 'echo gitdir: /tmp > .git'")"
  check "claude in a worktree without an environment doesn't run" no \
    "$(succeeds in_t3 "mkdir -p /var/lib/fern/worktrees/t3check/none && cd /var/lib/fern/worktrees/t3check/none && claude --version")"
  # A thread in T3's Local mode works in the main folder, which has no box.
  check "claude in a project's main folder answers T3's probes" "$CLAUDE" "$(in_t3 "cd $R && claude --version")"
  check "but a thread's session there is refused, naming New worktree" yes \
    "$(in_t3 "cd $R && claude --session-id 00000000-0000-4000-8000-000000000000 -p hi 2>&1; true" | grep -q 'New worktree' && echo yes || echo no)"
  # T3 clones a project from GitHub into its home unless told otherwise:
  # projects/, where an update of T3's image keeps it and a worktree's box
  # gets its git folder, but just as boxless.
  check "T3's home is projects/" /var/lib/fern/projects "$(in_t3 "cd \$HOME && pwd")"
  check "and so is one in a project T3 cloned into its home" yes \
    "$(in_t3 "mkdir -p \$HOME/fern-check-clone && cd \$HOME/fern-check-clone && claude --resume 00000000-0000-4000-8000-000000000000 -p hi 2>&1; true" | grep -q 'New worktree' && echo yes || echo no)"
  in_t3 "rm -rf \$HOME/fern-check-clone"
  check "T3's new threads are New worktrees, and deleting one removes it, untracked files and all" "worktree true tracked-changes" \
    "$(in_t3 "node -p 'const s = require(\"/var/lib/fern/userdata/settings.json\"); s.defaultThreadEnvMode + \" \" + s.storageCleanup.worktreeOnDelete + \" \" + s.storageCleanup.worktreeKeepWhen'")"
  check "T3 doesn't offer its browser, or write text with a Codex the image lacks" "false false" \
    "$(in_t3 "node -p 'const s = require(\"/var/lib/fern/userdata/settings.json\"); s.enableAgentBrowserAccess + \" \" + s.providerInstances.codex.enabled'")"
  in_t3 "echo v1 > $W/a.txt && echo junk > $W/ignored.bin"
  metric t3.checkpoint_ms "checkpoint snapshot, ms" "$(resource_op t3.checkpoint snapshot --replace t3c c1 | took)"
  in_t3 "echo v2 > $W/a.txt"
  metric t3.checkpoint_replaced_ms "checkpoint snapshot, replaced, ms" "$(resource_op t3.checkpoint_replaced snapshot --replace t3c c1 | took)"
  in_t3 "echo v3 > $W/a.txt && rm $W/ignored.bin"
  metric t3.restore_ms "restore, ms" "$(resource_op t3.restore restore t3c c1 | took)"
  check "restore brings back the replaced snapshot" v2 "$(in_t3 "cat $W/a.txt")"
  check "and a file git doesn't track" junk "$(in_t3 "cat $W/ignored.bin")"
  check "git works in the restored worktree" "M a.txt" "$(in_t3 "git -C $W status --short --untracked-files=no" | xargs)"
  check "Claude's state outlives the restore" kept "$(in_t3 "cd $W && fern exec $x -- cat $C/note")"
  # The login reaches the box from /etc/fern/claude.env, which a box reads
  # when it starts. Without one, a fake one shows the way works: Claude then
  # fails at Anthropic's API instead of asking for a login.
  if on_node test -s /etc/fern/claude.env </dev/null; then
    fern sleep t3c >/dev/null
    check "Claude's box has its login" yes \
      "$(succeeds in_t3 "cd $W && fern exec $x -- sh -c 'test -n \"\$CLAUDE_CODE_OAUTH_TOKEN\"'")"
    check "T3 has Claude's login" yes "$(succeeds in_t3 "test -n \"\$CLAUDE_CODE_OAUTH_TOKEN\"")"
  else
    echo CLAUDE_CODE_OAUTH_TOKEN=fern-lab-not-a-token | on_node sh -c 'umask 077 && cat >/etc/fern/claude.env'
    fern sleep t3c >/dev/null
    check "Claude in the box uses the login from /etc/fern/claude.env" "API Error: 401" \
      "$(in_t3 "cd $W && claude -p hi 2>&1" 2>/dev/null | grep -o 'API Error: 401' | head -1)"
    on_node rm /etc/fern/claude.env </dev/null
    note "no Claude login stored yet: lab/claude-login.sh"
  fi
  # The box with the services writes the workspace, its tools' folder
  # included, which that box has first on its PATH. Claude's box must not:
  # a file named claude there would run where Claude's login is. Until
  # 2026-11-13 it did.
  if [ "${AGENTS:-0}" = 1 ]; then
    in_t3 "cd $W && printf '[tools]\n' > mise.toml && mkdir -p .fern/tools/shims &&
      printf '%s\n' '#!/bin/sh' 'echo planted' > .fern/tools/shims/claude && chmod +x .fern/tools/shims/claude"
    fern sleep t3c >/dev/null
    check "a claude the workspace plants among its tools doesn't run in Claude's box" "$CLAUDE" "$(in_t3 "cd $W && claude --version")"
    check "no folder of the pool is on the PATH of Claude's box" 0 \
      "$(in_t3 "cd $W && fern exec --here --agent -- sh -c 'echo \$PATH'" | tr ':' '\n' | grep -c '^/var/lib/fern' || true)"
    in_t3 "cd $W && rm -rf mise.toml .fern/tools"
    fern sleep t3c >/dev/null
  fi
  # T3's own git, in a worktree where the box made a repository in a folder
  # and armed it: a filter in that folder's config, and a .gitmodules that
  # asks git to look into it. Staged as a submodule, as T3's own `git add`
  # stages it, git status, diff, add, commit -a, stash and fetch each start
  # a git in that folder, which would run the filter in T3's service
  # (found 2026-11-13). T3's git is fern-git, which opens no repository in
  # a thread's own folder.
  in_t3 "cd $W && fern exec -i --here -- sh -s make" <lab/nested-repo.sh
  in_t3 "cd $W && git add --all 2>/dev/null; git -c user.name=t -c user.email=t@fern commit -qm 'with a folder that has a repository'"
  in_t3 "cd $W && fern exec -i --here -- sh -s arm $W/filter-ran && echo more >> a.txt" <lab/nested-repo.sh
  check "the index holds the box's folder as a submodule, as T3's git add stages it" 1 "$(in_t3 "cd $W && git ls-files --stage | grep -c ^160000" || true)"
  in_t3 "cd $W && { git status --porcelain; git diff HEAD --stat; git add --all; git stash; git stash pop; git fetch --recurse-submodules=yes $R; git -c user.name=t -c user.email=t@fern commit -qam again; } >/dev/null 2>&1" || true
  check "T3's git ran none of what the box's repository names, through seven commands" no "$(succeeds in_t3 "test -e $W/filter-ran")"
  check "and still does its work in the worktree" "again" "$(in_t3 "cd $W && git log -1 --format=%s")"
  check "git in the box's folder itself is refused, with the reason" "128 fern-git: not opening the repository" \
    "$(in_t3 "cd $W/nested && git status >/dev/null 2>&1; echo \$?") $(in_t3 "cd $W/nested && git status 2>&1" | cut -c1-36)"
  in_t3 "cd $W && rm -rf nested .gitmodules filter-ran && git add --all && git -c user.name=t -c user.email=t@fern commit -qm tidy" >/dev/null 2>&1 || true

  # A terminal in T3 (fern-terminals.patch, images/fern-shell): a shell in
  # the thread's box, never one in T3's service, which has every worktree
  # and the server's logins. The pane's own calls, from inside the node: a
  # VPS's T3 has no port the Mac reaches without a tunnel. The worktree has
  # no environment yet, as before a thread's first checkpoint.
  local W2=/var/lib/fern/worktrees/t3check/two pair e2
  e2=t3-$(printf %s "$W2" | shasum -a 256 | cut -c1-16)
  fern rm "$e2" 2>/dev/null || true
  in_t3 "cd $R && git worktree add -q -b t3/two $W2"
  terminal() { # the folder, the line to type, what to wait for
    pair=$(in_t3 "node /opt/t3/apps/server/dist/bin.mjs pair --ttl 2m --label 'fern check'" </dev/null |
      sed -nE 's#^Pairing URL: http://[^/]+/#http://127.0.0.1:30773/#p')
    in_t3 "T3_PAIR_URL='$pair' T3_CWD='$1' T3_TYPE='$2' T3_EXPECT='$3' T3_RESIZE='97 31' node --input-type=module -" <lab/t3-terminal.mjs 2>&1
  }
  # In the box there is no daemon's socket, the login isn't in the
  # environment, and the terminal's size is the pane's.
  check "a terminal in a thread's worktree is a shell in its box, at the pane's size" "running | at=$W2 socket=no login=0 idle=1200 size=31 97" \
    "$(terminal "$W2" 'echo at=$PWD socket=$(test -e /run/fern/fern.sock && echo yes || echo no) login=$(env | grep -c ^CLAUDE_CODE_OAUTH) idle=$TMOUT size=$(stty size)' "at=/[^\r\n]* size=[0-9]+ [0-9]+")"
  # An open terminal keeps its box awake, so the shell ends at an idle
  # prompt: after 20 minutes, here after the 2 seconds it is given.
  check "the shell ends itself at an idle prompt, so the box can sleep" "exited | auto-logout" "$(terminal "$W2" 'TMOUT=2' 'auto-logout')"
  check "the worktree became the thread's environment for it" "$W2" "$(fern ls | awk -v id="$e2" '$1 == id { print $NF }')"
  check "a terminal in a project's main folder starts no shell, and says why" "exited | No terminal here: this folder has no Fern box" \
    "$(terminal "$R" '' 'No terminal here: this folder has no Fern box')"
  check "no shell runs in T3's service" 0 \
    "$(on_node sh -c 'for p in $(cat /sys/fs/cgroup/system.slice/fern-t3.service/cgroup.procs); do cat /proc/$p/comm; done' </dev/null | grep -cxE 'sh|bash|zsh|dash' || true)"
  fern rm "$e2" >/dev/null 2>&1 || true
  in_t3 "fern rm t3c && rm -rf $R /var/lib/fern/worktrees/t3check"
}

# hardware is deliberately alone: idle CPU needs a quiet node, and the
# platform sample must not include a concurrently running timing section.
# guess builds the TanStack Start repository without its services.yaml and
# mise.toml, with Postgres in a docker-compose.yml, as a repository that
# knows nothing of Fern: the services and tools are Fern's guess.
guess() {
  echo "== a repository without Fern's files: services and tools guessed"
  local W=/var/lib/fern/worktrees/guess-check G key
  G=$(golden_id guess-check)
  for e in guess1 guess2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  local compose='services:\n  db:\n    image: postgres:16\n    environment:\n      POSTGRES_USER: app\n      POSTGRES_PASSWORD: secret\n'
  for wt in one two; do
    tar_dir lab/tanstack-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - &&
      rm $W/$wt/services.yaml $W/$wt/mise.toml && printf '$compose' >$W/$wt/docker-compose.yml && chown -R 1000:1000 $W/$wt"
  done
  fern create guess1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 300); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric guess.golden_build_ms "golden build from a guess, ms" $(($(now_ms) - start))
  check "a golden snapshot is built from the guess" 1 "$(grep -c . <<<"$key")"
  if [ -z "$key" ]; then
    box_log "$G" | tail -20 | sed 's/^/  builder: /'
    return
  fi
  check "it holds the guessed tools, node_modules and the database, which git ignores" "yes yes yes yes yes" \
    "$(in_daemon "cd /var/lib/fern/envs/$G/snaps/$key && for p in .fern/tools/guess.toml .fern/tools/installs/pnpm node_modules/.pnpm .fern/pg/fern-ready .fern/pg/.gitignore; do test -e \$p && echo yes || echo no; done" | xargs)"
  fern create guess2 --path $W/two >/dev/null
  start=$(now_ms)
  check "a preview wakes the thread and the guessed dev server serves" yes \
    "$(succeeds grep -q 'Hello world' <<<"$(on_node curl -s -H "Host: guess2.localhost:30780" http://127.0.0.1:30780/ </dev/null)")"
  note "asleep to the page through the preview, from a guess: $(($(now_ms) - start)) ms"
  check "Postgres runs as docker-compose's, with its role and database" app \
    "$(fern exec guess2 -- psql -h 127.0.0.1 -U app app -tAc 'select current_user' 2>&1)"
  check "Bash gets mise's Node and pnpm" "yes yes" \
    "$(fern exec guess2 -- sh -c 'for t in node pnpm; do command -v $t | grep -q .fern/tools && echo yes || echo no; done' | xargs)"
  for e in guess1 guess2 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}

# cloudflare: an Astro site on Cloudflare with D1 and R2 and no Fern files
# (lab/cloudflare-repo). Fern guesses its dev server, which emulates D1 and
# R2 in the workspace; a revert brings back a deleted row and object; Claude
# is told; and the node's Cloudflare token reaches the agent box's Claude,
# never the box that runs the repository's code.
cloudflare() {
  echo "== a Cloudflare repository: D1 and R2 in snapshots"
  local W=/var/lib/fern/worktrees/cloudflare-check G key secret=cf-check-token-$RANDOM saved=no
  G=$(golden_id cloudflare-check)
  for e in cf1 cf2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  for wt in one two; do
    tar_dir lab/cloudflare-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - && chown -R 1000:1000 $W/$wt"
  done
  # The first thread starts the golden build; the second starts from it.
  fern create cf1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 300); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric cloudflare.golden_build_ms "golden build of an Astro site on Cloudflare, ms" $(($(now_ms) - start))
  check "a golden snapshot is built from the guess" 1 "$(grep -c . <<<"$key")"
  if [ -z "$key" ]; then
    box_log "$G" | tail -20 | sed 's/^/  builder: /'
    return
  fi
  # Every pack on for the node, the node's own choice kept aside.
  local skills_saved=no
  if in_daemon "test -e /etc/fern/skills"; then
    in_daemon "cp -p /etc/fern/skills /etc/fern/skills.check" && skills_saved=yes
  fi
  on_node sh -c 'cat >/tmp/fern-install-skills.sh' <install.sh
  on_node bash /tmp/fern-install-skills.sh skills "beginner design engineering safety" >/dev/null
  in_daemon "rm /tmp/fern-install-skills.sh"
  fern create cf2 --path $W/two >/dev/null
  # api METHOD PATH [BODY] calls the site through the preview, which wakes
  # the box, and prints the body.
  api() { on_node curl -s -X "$1" -H "Host: cf2.localhost:30780" --data-raw "${3:-}" "http://127.0.0.1:30780$2" </dev/null; }
  start=$(now_ms)
  check "a preview wakes the thread and the guessed dev server serves" yes \
    "$(succeeds grep -q '<title>Scores</title>' <<<"$(api GET /)")"
  metric cloudflare.first_page_ms "asleep to the first page through the preview, from the golden, ms" $(($(now_ms) - start))
  api POST /api/scores '{"name":"ada","points":3}' >/dev/null
  api PUT /api/pic green >/dev/null
  check "D1 and R2 work, with no account" '[{"name":"ada","points":3}] green' "$(api GET /api/scores) $(api GET /api/pic)"
  check "their data is in the workspace" yes "$(succeeds in_daemon "test -d $W/two/.wrangler/state/v3/d1")"
  fern snapshot cf2 c1 >/dev/null
  api DELETE /api/scores >/dev/null
  api DELETE /api/pic >/dev/null
  check "deleted" "[] none" "$(api GET /api/scores) $(api GET /api/pic)"
  fern restore cf2 c1 >/dev/null
  check "a revert brings back the D1 row and the R2 object" '[{"name":"ada","points":3}] green' "$(api GET /api/scores) $(api GET /api/pic)"

  if [ "$AGENTS" = 1 ]; then
    # A Cloudflare token on the node, the real one kept aside.
    if in_daemon "test -e /etc/fern/cloudflare.env"; then
      in_daemon "cp -p /etc/fern/cloudflare.env /etc/fern/cloudflare.env.check" && saved=yes
    fi
    on_node sh -c 'cat >/tmp/fern-install-check.sh' <install.sh
    printf '%s\n' "$secret" | on_node bash /tmp/fern-install-check.sh cloudflare >/dev/null
    in_daemon "rm /tmp/fern-install-check.sh"
    # An agent is a program named claude: this one prints its arguments.
    in_daemon "mkdir -p $W/two/.fern/bin && printf '#!/bin/sh\nprintf \"%%s\\\\n\" \"\$@\"\n' >$W/two/.fern/bin/claude && chmod 755 $W/two/.fern/bin/claude && chown -R 1000:1000 $W/two/.fern/bin"
    check "Claude in the agent box gets Cloudflare's MCP server with the token" yes \
      "$(fern exec --agent cf2 -- $W/two/.fern/bin/claude -p | grep 'mcp.cloudflare.com' | grep -q "Bearer $secret" && echo yes || echo no)"
    # A pattern that doesn't match its own grep's command line.
    check "and, in an Astro site, Astro's docs server" yes \
      "$(fern exec --agent cf2 -- $W/two/.fern/bin/claude -p | grep -q 'mcp.docs.astro.build' && echo yes || echo no)"
    check "the box that runs the repository's code never sees it" no \
      "$(succeeds fern exec cf2 -- sh -c "grep -qs '${secret:0:1}[${secret:1:1}]${secret:2}' /proc/[0-9]*/environ /proc/[0-9]*/cmdline")"
    check "nor does the workspace" no "$(succeeds in_daemon "grep -rqs $secret $W /var/lib/fern/envs/cf2")"
    if [ $saved = yes ]; then
      in_daemon "mv /etc/fern/cloudflare.env.check /etc/fern/cloudflare.env"
    else
      in_daemon "rm -f /etc/fern/cloudflare.env"
    fi
  fi
  # Claude's box, the agent box if there is one, wrote the hint as it woke;
  # the skills are links into its image.
  local claude_box=""
  [ "$AGENTS" = 1 ] && claude_box=--agent
  check "Claude is told it is a Cloudflare repository" yes \
    "$(succeeds in_daemon "grep -q 'deploys to Cloudflare' /var/lib/fern/envs/cf2/claude/CLAUDE.md")"
  check "and gets Cloudflare's skills, fern-deploy and a skill of every pack the node turns on" "yes yes yes yes yes yes" \
    "$(fern exec $claude_box cf2 -- sh -c 'cd "$CLAUDE_CONFIG_DIR/skills" && for s in fern-deploy wrangler frameworks-on-cloudflare animate coding-standards site-safety; do test -f $s/SKILL.md && echo yes || echo no; done' | xargs)"
  check "and the beginner pack's note" yes \
    "$(succeeds in_daemon "grep -q \"isn't a developer\" /var/lib/fern/envs/cf2/claude/CLAUDE.md")"
  if [ $skills_saved = yes ]; then
    in_daemon "mv /etc/fern/skills.check /etc/fern/skills"
  else
    in_daemon "rm -f /etc/fern/skills"
  fi
  for e in cf1 cf2 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}

# github sets up a GitHub App with a new key, points the daemon at a fake
# GitHub on the node that answers as GitHub's API does for an App installed
# on owner/app, and asks for credentials as T3's git and gh do.
# next: a Next.js app with Postgres in docker-compose.yml and no Fern files
# (lab/next-repo), from a lockfile npm installs, to its page reading the
# database through the DATABASE_URL the guess sets (roadmap step 1).
next() {
  echo "== Next.js without Fern's files"
  local W=/var/lib/fern/worktrees/next-check G key page
  G=$(golden_id next-check)
  for e in next1 next2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  for wt in one two; do
    tar_dir lab/next-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - && chown -R 1000:1000 $W/$wt"
  done
  fern create next1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 600); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric next.golden_build_ms "golden build of the Next.js app, ms" $(($(now_ms) - start))
  check "a golden snapshot is built from the guess" 1 "$(grep -c . <<<"$key")"
  if [ -z "$key" ]; then
    box_log "$G" | tail -20 | sed 's/^/  builder: /'
    return
  fi
  check "it holds npm's node_modules, Next and the database" "yes yes" \
    "$(in_daemon "cd /var/lib/fern/envs/$G/snaps/$key && for p in node_modules/next .fern/pg/fern-ready; do test -e \$p && echo yes || echo no; done" | xargs)"
  fern create next2 --path $W/two >/dev/null
  start=$(now_ms)
  page=$(on_node curl -s -m 300 -H "Host: next2.localhost:30780" http://127.0.0.1:30780/ </dev/null | sed 's/<!-- -->//g')
  metric next.first_page_ms "asleep to Next.js's first page through the preview, ms" $(($(now_ms) - start))
  check "the page reads docker-compose's database as its user" yes \
    "$(succeeds grep -q 'Hello from Next.js, database user app' <<<"$page")"
  if ! grep -q 'Hello from Next' <<<"$page"; then
    box_log next2 | tail -20 | sed 's/^/  next2: /'
  fi
  start=$(now_ms)
  on_node curl -s -m 60 -o /dev/null -H "Host: next2.localhost:30780" http://127.0.0.1:30780/ </dev/null
  metric next.page_ms "the page again, ms" $(($(now_ms) - start))
  for e in next1 next2 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}

github() {
  echo "== T3's git and gh get tokens from the daemon's GitHub App"
  local args dir=/tmp/fern-github-check
  args=$(daemon_args)
  in_daemon "set -e; rm -rf $dir; mkdir -p $dir/app; echo 7 >$dir/app/id
    openssl genrsa -traditional -out $dir/app/key.pem 2048 2>/dev/null
    cat >$dir/fake.py <<'PY'
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def reply(self, code, body):
        self.send_response(code); self.send_header('Content-Type', 'application/json'); self.end_headers()
        self.wfile.write(json.dumps(body).encode())
    def do_GET(self):
        if not self.headers.get('Authorization', '').startswith('Bearer ey'):
            self.reply(401, {'message': 'Bad credentials'})
        elif self.path == '/repos/owner/app/installation':
            self.reply(200, {'id': 42})
        elif self.path.startswith('/app/installations?'):
            self.reply(200, [{'id': 42, 'account': {'login': 'owner'}, 'permissions': {'contents': 'write', 'pull_requests': 'write'}}])
        else:
            self.reply(404, {'message': 'Not Found'})
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path != '/app/installations/42/access_tokens':
            self.reply(400, {})
        elif body.get('repositories') == ['app']:
            self.reply(201, {'token': 'ghs_check', 'expires_at': '2099-01-01T00:00:00Z'})
        elif 'repositories' not in body:
            self.reply(201, {'token': 'ghs_account', 'expires_at': '2099-01-01T00:00:00Z'})
        else:
            self.reply(400, {})
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', 18080), H).serve_forever()
PY
    (python3 $dir/fake.py >/dev/null 2>&1 &) ; sleep 0.5"
  set_daemon_args $args --github-app=$dir/app --github-api=http://127.0.0.1:18080
  local ask='printf "protocol=https\nhost=github.com\npath=owner/app.git\n\n" | git credential fill'
  check "T3's git gets a token for a repository with the App" password=ghs_check \
    "$(in_t3 "$ask" 2>&1 | grep '^password=')"
  check "and none for one without it" "" \
    "$(in_t3 "${ask/owner\/app/owner/other} </dev/null 2>/dev/null; true" 2>&1 | grep '^password=' || true)"
  check "T3's gh gets one through its wrapper" ghs_check "$(in_t3 'fern github-token owner/app' 2>&1)"
  check "T3's image has GitHub's CLI" yes "$(succeeds in_t3 'gh --version >/dev/null')"
  # T3 asks for its GitHub credential this way, in its own folder, once for
  # the host, and uses it for every thread's pull requests.
  check "T3's own question for a credential gets the account's token" ghs_account \
    "$(in_t3 'cd /opt/t3 && gh auth token --hostname github.com' 2>&1)"
  set_daemon_args $args
  in_daemon "pkill -f $dir/fake.py; rm -rf $dir"
}

# backup runs versitygw on the node as the object store, backs a thread up to
# it, changes the thread's data, backs up again (an incremental send), and
# restores the bucket into a scratch pool: the restored workspace must be
# the backed-up one, file for file.
backup() {
  echo "== threads backed up to an object store, and restored"
  if on_node test -e /etc/fern/backup.env </dev/null; then
    note "skipped: this node has backups set up already (/etc/fern/backup.env)"
    return
  fi
  local args scratch=/tmp/fern-restore node
  args=$(daemon_args)
  node=$(on_node hostname </dev/null)
  in_daemon "podman rm -f fern-s3 >/dev/null 2>&1; podman run -d -q --name fern-s3 --network=host --tmpfs /data \
    docker.io/versity/versitygw:latest --access fernkey --secret fernsecret --port 127.0.0.1:19000 posix /data >/dev/null
    for i in \$(seq 50); do curl -s -o /dev/null http://127.0.0.1:19000/ && break; sleep 0.2; done
    curl -sf --aws-sigv4 aws:amz:us-east-1:s3 --user fernkey:fernsecret -X PUT http://127.0.0.1:19000/fern-check
    umask 077; printf 'FERN_BACKUP_ENDPOINT=http://127.0.0.1:19000\nFERN_BACKUP_REGION=us-east-1\nFERN_BACKUP_BUCKET=fern-check\nFERN_BACKUP_ACCESS_KEY=fernkey\nFERN_BACKUP_SECRET_KEY=fernsecret\nFERN_BACKUP_KEY=%s\n' \$(openssl rand -hex 32) >/etc/fern/backup.env"
  set_daemon_args $args --backup-every=24h
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Backed', 'up@example.com')" >/dev/null
  fern sleep $ENV >/dev/null
  objects() { in_daemon "curl -s --aws-sigv4 aws:amz:us-east-1:s3 --user fernkey:fernsecret 'http://127.0.0.1:19000/fern-check?list-type=2&prefix=fern/$node/envs/$ENV/'" | grep -o '<Key>' | wc -l | tr -d ' '; }
  check "fern backup backs the thread up" 0 "$(fern backup >/dev/null 2>&1; echo $?)"
  check "… as one send" 1 "$(objects)"
  sql $ENV "insert into users (name, email) values ('Again', 'again@example.com')" >/dev/null
  fern sleep $ENV >/dev/null
  check "a change, and the next backup sends it from the first" 2 "$(fern backup >/dev/null 2>&1; objects)"
  check "an unchanged thread isn't sent again" 2 "$(fern backup >/dev/null 2>&1; objects)"
  in_daemon "set -e; umount $scratch 2>/dev/null || true; rm -rf $scratch $scratch.img; mkdir -p $scratch
    truncate -s 4G $scratch.img && mkfs.btrfs -q $scratch.img && mount -o loop $scratch.img $scratch"
  check "the backup restores into another pool" 0 "$(in_daemon "fern-daemon restore $node $scratch >/dev/null 2>&1; echo \$?")"
  local snap
  snap=$(in_daemon "ls /var/lib/fern/envs/$ENV/snaps | grep '^fern-backup-' | tail -1")
  check "… the workspace file for file" "$(treehash /var/lib/fern/envs/$ENV/snaps/$snap)" "$(treehash $scratch/envs/$ENV/live)"
  check "… and its record and T3's data with it" "yes yes" \
    "$(in_daemon "for p in envs/$ENV/env.json userdata; do test -e $scratch/\$p && echo yes || echo no; done" | xargs)"
  in_daemon "umount $scratch; rm -rf $scratch $scratch.img; rm -f /etc/fern/backup.env; podman rm -f fern-s3 >/dev/null"
  set_daemon_args $args
  fern rm $ENV
  # The node's other threads were backed up too: each keeps its last
  # backup snapshot, as the next backup's parent.
  local e s
  for e in $(fern ls | awk 'NR > 1 { print $1 }'); do
    for s in $(fern snapshots "$e" 2>/dev/null | awk '/^fern-backup-/ { print $1 }'); do fern rmsnap "$e" "$s" >/dev/null; done
  done
}

# go_seconds prints a Go duration (5m, 1m30s, 250ms) in seconds.
go_seconds() {
  awk -v d="$1" 'BEGIN {
    while (match(d, /^[0-9.]+(ms|h|m|s)/)) {
      u = substr(d, 1, RLENGTH); d = substr(d, RLENGTH + 1); n = u + 0; sub(/^[0-9.]+/, "", u)
      s += n * (u == "h" ? 3600 : u == "m" ? 60 : u == "s" ? 1 : 0.001)
    }
    print s + 0 }'
}
# idle_policy prints, in seconds, how long Fern lets an idle Claude run
# before it stops it (fern-claude's FERN_HIBERNATE; 0: never), how long T3
# keeps Claude after a turn, and the daemon's agent and human idle times, as
# deployed.
idle_policy() {
  local args t3 hibernate
  local env
  args=$(daemon_args)
  env=$(on_node systemctl show -p Environment --value fern-t3 </dev/null | tr ' ' '\n')
  t3=$(sed -n 's/^T3CODE_PROVIDER_IDLE_TIMEOUT_MS=//p' <<<"$env")
  hibernate=$(sed -n 's/^FERN_HIBERNATE=//p' <<<"$env")
  # An older T3 image's fern-claude doesn't hibernate.
  in_t3 'grep -q -- --hibernate /usr/local/bin/claude' 2>/dev/null || hibernate=0
  echo "$(go_seconds "${hibernate:-30s}")" "$((${t3:-300000} / 1000))" \
    "$(go_seconds "$(tr ' ' '\n' <<<"$args" | sed -nE 's/^--?idle-agent=//p' | grep . || echo 5m)")" \
    "$(go_seconds "$(tr ' ' '\n' <<<"$args" | sed -nE 's/^--?idle-human=//p' | grep . || echo 20m)")"
}
# res_value prints metric $2 from resource_op's output in file $1.
res_value() { awk -v k="$2" '$1 == "METRIC" && $2 == k { print $3 }' "$1"; }

# turn measures what one agent turn costs the node, the way T3 runs it: the
# box wakes, Claude starts and makes a few tool calls, then waits until T3
# ends it, and the box sleeps. Claude is the real binary waiting for its next
# stream-json message, as T3 keeps it between turns; it needs no login until
# a message comes. The idle part isn't waited out: its CPU rate and memory
# are measured over TURN_IDLE_SECONDS, and how long it lasts comes from the
# deployed idle times (architecture §6.3): Fern stops an idle Claude after
# FERN_HIBERNATE and the box sleeps at once; without hibernation T3 ends
# Claude after its idle timeout, and the box sleeps the daemon's agent time
# later, or its human time after the turn started if that is later.
turn() {
  echo "== a turn: wake, tool calls, idle, sleep"
  local seconds=${TURN_IDLE_SECONDS:-30} uid claude hibernate t3 agent human awake wake_cpu tools_cpu sleep_cpu
  fresh $ENV
  metric turn.wake_ms "a turn's wake, ms" "$(resource_op turn.wake wake $ENV 2>"$TMP/wake" | took)"
  cat "$TMP/wake"
  # In a session of its own where there is setsid (the VPS), which the end
  # of SSH's doesn't hang up: then its process group is what to stop. The
  # lab's node, reached without SSH, has none: there fern exec itself.
  # shellcheck disable=SC2016 # expanded on the node
  claude=$(on_node sh -c 'claude="claude -p --input-format stream-json --output-format stream-json --verbose"
    if command -v setsid >/dev/null; then
      setsid sh -c '\''sleep "$1" | "$2" exec -i "$3" -- $4'\'' sh "$@" "$claude" >/dev/null 2>&1 </dev/null &
      echo "-$!"
    else
      sleep "$1" | "$2" exec -i "$3" -- $claude >/dev/null 2>&1 &
      echo "$!"
    fi' sh $((seconds + 120)) "$FERN_CLI" $ENV </dev/null)
  # Tool calls, as Claude's Bash tool makes them: one process after another.
  # shellcheck disable=SC2016 # expanded in the box
  metric turn.tools_ms "six tool calls (git, grep, read, psql, write, diff), ms" "$(resource_op turn.tools exec $ENV -- sh -c '
    s=$(date +%s%N)
    git status --short >/dev/null 2>&1; grep -rn users . >/dev/null; cat services.yaml >/dev/null
    psql -tAc "select count(*) from users" >/dev/null; echo "-- a turn" >>notes.md; git diff --stat >/dev/null 2>&1
    echo $((($(date +%s%N) - s) / 1000000))' 2>"$TMP/tools")"
  cat "$TMP/tools"
  check "Claude waits in the box" yes "$(succeeds fern exec $ENV -- pgrep -x claude)"
  sleep 5
  uid=$(pod_uid $ENV)
  hardware_uids
  hardware_sample --counters "$uid" >"$TMP/idle-before"
  sleep "$seconds"
  hardware_sample --counters "$uid" >"$TMP/idle-after"
  resource_rates turn.idle "$TMP/idle-before" "$TMP/idle-after" | tee "$TMP/rates"
  metric turn.box_memory_mib "box memory with Claude waiting, MiB (anon + shmem)" \
    "$(awk '$1 == "box_memory_mib" { print $2 }' "$TMP/idle-after")"
  # T3 ends Claude: fern exec passes SIGTERM on to it.
  on_node kill -TERM -- "$claude" </dev/null
  metric turn.sleep_ms "a turn's sleep, ms" "$(resource_op turn.sleep sleep $ENV 2>"$TMP/sleep" | took)"
  cat "$TMP/sleep"
  read -r hibernate t3 agent human <<<"$(idle_policy)"
  awake=$(awk -v z="$hibernate" -v t="$t3" -v a="$agent" -v h="$human" 'BEGIN { print (z > 0 ? z : t + a > h ? t + a : h) }')
  metric turn.awake_after_s "how long a box stays awake after a short turn, s (deployed idle times)" "$awake"
  wake_cpu=$(res_value "$TMP/wake" turn.wake.node_cpu_ms) tools_cpu=$(res_value "$TMP/tools" turn.tools.node_cpu_ms)
  sleep_cpu=$(res_value "$TMP/sleep" turn.sleep.node_cpu_ms)
  metric turn.cpu_ms "CPU of a turn, ms: wake, tools and sleep (node), then the box's while it waits" \
    "$(awk -v w="$wake_cpu" -v t="$tools_cpu" -v s="$sleep_cpu" -v a="$awake" \
      -v r="$(res_value "$TMP/rates" turn.idle.box_cpu_per_min_ms)" \
      'BEGIN { printf "%.0f", w + t + s + r * a / 60 }')"
  metric turn.box_mib_minutes "memory held by a turn's box while it waits, MiB x minutes" \
    "$(awk -v m="$(awk '$1 == "box_memory_mib" { print $2 }' "$TMP/idle-after")" -v a="$awake" 'BEGIN { printf "%.0f", m * a / 60 }')"
}

hardware() {
  echo "== hardware: idle CPU, fixed platform cost and allocated pool extents"
  local uid empty_uid before_bytes after_bytes seconds=${CPU_IDLE_SECONDS:-15}
  hardware_uids
  hardware_sample >"$TMP/platform-before"
  sleep "$seconds"
  hardware_sample >"$TMP/platform-after"
  resource_rates hardware.platform_idle "$TMP/platform-before" "$TMP/platform-after"
  while read -r name value; do
    case $name in
      t3_*mib|daemon_*mib|daemon_*count)
        metric "hardware.platform.$name" "idle platform $name" "$value" ;;
    esac
  done <"$TMP/platform-after"
  fresh $ENV
  resource_op box.fresh_wake wake $ENV >/dev/null
  fern rm ${ENV}-empty 2>/dev/null || true
  fern create ${ENV}-empty >/dev/null
  fern wake ${ENV}-empty >/dev/null
  uid=$(pod_uid $ENV) empty_uid=$(pod_uid ${ENV}-empty)
  # Let initdb's startup activity settle before the idle interval.
  sleep 2
  hardware_sample --counters "$uid" >"$TMP/pg-before"
  hardware_sample --counters "$empty_uid" >"$TMP/empty-before"
  sleep "$seconds"
  hardware_sample --counters "$uid" >"$TMP/pg-after"
  hardware_sample --counters "$empty_uid" >"$TMP/empty-after"
  resource_rates hardware.postgres_idle "$TMP/pg-before" "$TMP/pg-after"
  resource_rates hardware.empty_idle "$TMP/empty-before" "$TMP/empty-after"
  fern rm ${ENV}-empty
  # btrfs du cannot see delayed allocations. Flush initdb and seed writes
  # before attributing any later allocation growth to checkpoints.
  in_daemon 'sync -f /var/lib/fern'
  pool_du hardware.environment "/var/lib/fern/envs/$ENV/live"
  before_bytes=$(in_daemon "btrfs filesystem du -s --raw /var/lib/fern/envs/$ENV" | awk 'NR>1 {print $2+$3}')
  for i in $(seq 5); do
    fern exec $ENV -- sh -c 'head -c 1048576 /dev/urandom > checkpoint-data'
    resource_op hardware.snapshot snapshot $ENV "disk-$i" >/dev/null
  done
  in_daemon 'sync -f /var/lib/fern'
  pool_du hardware.snapshot "/var/lib/fern/envs/$ENV/snaps/disk-5"
  pool_du hardware.environment_with_snapshots "/var/lib/fern/envs/$ENV"
  after_bytes=$(in_daemon "btrfs filesystem du -s --raw /var/lib/fern/envs/$ENV" | awk 'NR>1 {print $2+$3}')
  metric hardware.checkpoint_growth_bytes "allocated data growth/checkpoint (1 MiB replaced each time), bytes" \
    "$(awk -v a="$after_bytes" -v b="$before_bytes" 'BEGIN {printf "%.0f",(a-b)/5}')"
  # Counter reads and pod lookups change the cadence between wakes and
  # sleeps. Keep CPU samples separate from box's original timing loop.
  for _ in $(seq 3); do
    resource_op box.sleep sleep $ENV >/dev/null
    resource_op box.wake wake $ENV >/dev/null
  done
}

# Fifty lifecycle cycles are too long for quick or the repeated default
# gate. This is an all-only stability check; LEAK_CYCLES permits a smoke run.
stability() {
  echo "== lifecycle stability: ${LEAK_CYCLES:-50} wake/sleep cycles"
  hardware_uids
  fresh $ENV
  fern wake $ENV >/dev/null
  fern sleep $ENV >/dev/null
  sleep 2
  hardware_sample >"$TMP/before"
  metrics | awk '$1=="go_goroutines" {print "daemon_goroutines_count",$2}' >>"$TMP/before"
  for _ in $(seq "${LEAK_CYCLES:-50}"); do
    fern wake $ENV >/dev/null
    fern sleep $ENV >/dev/null
  done
  sleep 2
  hardware_sample >"$TMP/after"
  metrics | awk '$1=="go_goroutines" {print "daemon_goroutines_count",$2}' >>"$TMP/after"
  resource_stability stability "$TMP/before" "$TMP/after"
  check "no pod remains after the cycles" "" "$(pod_uid $ENV)"
  check "no workspace mount remains after the cycles" 0 "$(mounts $ENV)"
}

# notify gives the daemon a URL to tell (--notify-url) and a receiver on the
# node that keeps what is POSTed to it, breaks a golden build, and checks
# that the failure is told once, not again by a restarted daemon, and that
# the daemon says so when nothing is wrong any more. The daemon looks once a
# minute, so this takes four.
notify() {
  echo "== a failure is told to a URL, once"
  local W=/var/lib/fern/worktrees/ck-notify G args got=/tmp/fern-notified
  G=$(golden_id ck-notify)
  args=$(daemon_args)
  for e in $ENV ${ENV}2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  in_daemon "rm -f $got /var/lib/fern/notified.json; pkill -f fern-notify-receiver || true"
  # The receiver appends each POST's body, on a line of its own.
  on_node sh -c "nohup python3 -c '
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get(\"Content-Length\", 0)))
        open(\"$got\", \"ab\").write(body.replace(b\"\\n\", b\" \") + b\"\\n\")
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer((\"127.0.0.1\", 8099), H).serve_forever()  # fern-notify-receiver
' >/dev/null 2>&1 &" </dev/null
  # shellcheck disable=SC2086 # the daemon's arguments are words
  set_daemon_args $args --notify-url=http://127.0.0.1:8099/fern
  restart_daemon
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/one && mkdir -p $W/one/.fern && tar -x -C $W/one -f - &&
    printf '%s\n' '#!/bin/sh' 'echo no such tool: frobnicate >&2' 'exit 3' > $W/one/.fern/setup &&
    chmod +x $W/one/.fern/setup && chown -R 1000:1000 $W/one"
  fern create $ENV --path $W/one >/dev/null
  for _ in $(seq 150); do
    in_daemon "grep -q 'golden build' $got" 2>/dev/null && break
    sleep 1
  done
  check "a failed golden build is POSTed to the URL, with the builder's name" 1 "$(in_daemon "grep -c 'golden build.*$G' $got" || true)"
  restart_daemon
  sleep 75
  check "a restarted daemon doesn't tell it again" 1 "$(in_daemon "grep -c 'golden build' $got" || true)"
  # Mended, the next worktree's build works, and the last problem is gone.
  in_daemon "printf '%s\n' '#!/bin/sh' 'true' > $W/one/.fern/setup"
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/two && mkdir -p $W/two/.fern && tar -x -C $W/two -f - &&
    cp -p $W/one/.fern/setup $W/two/.fern/setup && chown -R 1000:1000 $W/two"
  fern create ${ENV}2 --path $W/two >/dev/null
  for _ in $(seq 200); do
    in_daemon "grep -q 'nothing is wrong any more' $got" 2>/dev/null && break
    sleep 1
  done
  check "and says when nothing is wrong any more" 1 "$(in_daemon "grep -c 'nothing is wrong any more' $got" || true)"
  # shellcheck disable=SC2086
  set_daemon_args $args
  restart_daemon
  in_daemon "pkill -f fern-notify-receiver || true; rm -f $got"
  for e in $ENV ${ENV}2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  in_daemon "rm -rf $W"
}

# main runs the sections. It is one function, called on the script's last
# line, so bash has read all of it before it starts: editing the script
# while it runs can't change what it does.
main() {
  all=(slice box disk space stream idle_exec concurrency restore fork pgload idle daemon upgrade settings node many scale sleeping limits sizes paths golden tools guess next cloudflare backup github claude opencode commit devserver turn hardware stability notify)
  quick=(slice box stream restore paths golden t3)
  # These restart the daemon or the node, change the daemon's arguments or
  # load the node with many boxes, so they run alone, after the others.
  alone=" idle daemon upgrade sizes node many scale sleeping turn hardware stability github devserver backup notify "
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
