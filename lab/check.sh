#!/usr/bin/env bash
# Checks Fern end to end in the lab, in sections:
#   slice        the whole disk, database included, is snapshotted and restored,
#                and a restore can be undone
#   box          a box runs under gVisor and keeps no state of its own: only
#                its workspace survives a sleep, and a force-deleted pod
#                comes back
#   disk         a 100,000-file workspace: snapshot, restore (equal to its
#                snapshot) and rm (nothing left); a seeded create; a real clone
#   stream       fern exec keeps a stream intact: bytes, stderr, exit codes,
#                stdin, signals, cancellation, long idle commands
#   concurrency  parallel execs on an awake and on a sleeping environment
#   restore      a restore while an exec is open; a missing snapshot
#   pgload       Postgres recovers, consistent, from every one of 60 snapshots
#                taken under pgbench's write load
#   idle         an idle box goes to sleep, with the daemon's idle times
#                shortened for the section; a running command keeps it awake
#   daemon       the daemon restarts while a box is awake, and finishes an rm
#                whose client went away
#   node         the whole node (k3s) restarts
#   many         ten environments woken at once
#   limits       what a box can't do: gain privileges, use more than its CPU
#                and PIDs, reach another box or anything private; and another
#                pod can't mount an environment
#   paths        workspaces at a path, as T3's worktrees: adopt, which,
#                exec --here, snapshot replace and delete
#   claude       Claude Code in a box: network, stream-json, its terminal UI
#   t3           T3's server: the checkpoint hook's commands on a worktree it
#                made, and that worktree's box with git, Claude's folder and
#                login. Needs lab/build.sh t3; not in the default run.
# Run all but t3, or name some: lab/check.sh stream daemon. Needs lab/up.sh,
# or lab/vps.sh and FERN_HOST for a VPS.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
ENV=ck IDLE=${IDLE:-180}
CLAUDE="$(sed -n 's/^ARG CLAUDE_VERSION=//p' images/Dockerfile) (Claude Code)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

FAILED=0
# check NAME WANT GOT records one pass or fail.
check() {
  if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: want '$2', got '$3'"; FAILED=1; fi
}
note() { echo "NOTE $*"; }
# succeeds prints yes if the command succeeds, no if it fails.
succeeds() { if "$@" >/dev/null 2>&1; then echo yes; else echo no; fi; }
# now_ms prints the wall clock in milliseconds.
now_ms() { local t=${EPOCHREALTIME//[!0-9]/}; echo $((t / 1000)); }
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
metrics() {
  local pod
  pod=$(k -n fern-system get pod -l app.kubernetes.io/name=fern -o jsonpath='{.items[0].metadata.name}')
  k get --raw "/api/v1/namespaces/fern-system/pods/$pod:9090/proxy/metrics"
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
# pod_cgroup prints file $2 of pod $1's cgroup on the node, which kubelet
# sets and the box can't see. Its name depends on the cgroup driver:
# pod<uid> in the lab, ...-pod<uid with _>.slice under systemd.
pod_cgroup() {
  local uid
  uid=$(pod_uid "$1")
  on_node sh -c "cat /sys/fs/cgroup/kubepods*/pod$uid/$2 /sys/fs/cgroup/kubepods*/*/pod$uid/$2 \
    /sys/fs/cgroup/kubepods*/*/*pod${uid//-/_}.slice/$2 /sys/fs/cgroup/kubepods*/*pod${uid//-/_}.slice/$2 2>/dev/null" </dev/null
}
# in_daemon runs a shell command in the daemon container, which sees the pool.
in_daemon() { k -n fern-system exec -i ds/fern -c fern -- sh -c "$1"; }
pod_uid() { k -n fern-boxes get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null || true; }
# wait_daemon waits until the daemon answers.
wait_daemon() {
  k -n fern-system rollout status ds/fern --timeout=300s >/dev/null
  until fern ls >/dev/null 2>&1; do sleep 1; done
}
# restart_node restarts k3s, and with it every pod.
restart_node() {
  if [ -n "$FERN_HOST" ]; then
    on_node sh -c 'k3s-killall.sh >/dev/null 2>&1; systemctl start k3s' </dev/null
  else
    docker restart "$LAB" >/dev/null
  fi
  until k get nodes 2>/dev/null | grep -q ' Ready'; do sleep 1; done
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

# mounts prints how often an environment's workspace is mounted on the node.
mounts() { on_node grep -c "/envs/$1/live " /proc/self/mountinfo </dev/null || true; }
# procs prints the names of the processes in an environment's box.
procs() { fern exec "$1" -- sh -c 'cat /proc/[0-9]*/comm 2>/dev/null; true' | sort -u; }

box() {
  echo "== a box keeps no state of its own"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Edsger', 'edsger@example.com')" >/dev/null
  # setsid: an exec's own process group ends with it.
  fern exec $ENV -- sh -c 'echo x > /tmp/outside && echo y > inside && setsid -f sleep 1000 >/dev/null 2>&1'
  check "the box runs under gVisor" "Starting gVisor..." "$(fern exec $ENV -- dmesg | head -1 | cut -c16-)"
  check "the pod belongs to its Sandbox" Sandbox \
    "$(k -n fern-boxes get pod $ENV -o jsonpath='{.metadata.ownerReferences[0].kind}')"
  check "the root filesystem is read-only" no "$(succeeds fern exec $ENV -- touch /etc/x)"
  check "the box writes to the pool" y "$(in_daemon "cat /var/lib/fern/envs/$ENV/live/inside")"
  check "a stray process runs" 1 "$(procs $ENV | grep -cx sleep || true)"
  local mounted=0
  metrics >"$TMP/metrics"
  for _ in $(seq 30); do
    fern sleep $ENV | took >>"$TMP/sleep"
    mounted=$((mounted + $(mounts $ENV)))
    fern wake $ENV | took >>"$TMP/wake"
  done
  note "sleep (until the pod is gone), ms: $(stats <"$TMP/sleep")"
  note "wake (until Postgres is ready), ms: $(stats <"$TMP/wake")"
  note "wake steps: $(wake_steps "$TMP/metrics")"
  check "the workspace is unmounted at every sleep" 0 "$mounted"
  check "and mounted once while awake" 1 "$(mounts $ENV)"
  check "a write outside the workspace is gone" no "$(succeeds fern exec $ENV -- test -e /tmp/outside)"
  check "a write in it stays" y "$(fern exec $ENV -- cat inside)"
  check "and so does the database (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
  check "only the declared services come back" 0 "$(procs $ENV | grep -cx sleep || true)"
  sql $ENV "insert into users (name, email) values ('Barbara', 'barbara@example.com')" >/dev/null
  start=$(now_ms)
  k -n fern-boxes delete pod $ENV --force --grace-period=0 >/dev/null 2>&1
  # A command sent while the pod is being killed can reach it and be cut off
  # (seen under gVisor in the lab); it isn't retried, as it may have run.
  # The next one reaches the new pod.
  check "Postgres recovers after its pod is force-deleted (5 users)" 5 \
    "$(sql $ENV 'select count(*) from users' 2>/dev/null || sql $ENV 'select count(*) from users')"
  note "force delete until Postgres answers: $(($(now_ms) - start)) ms"
  check "and the seed ran once" 1 "$(sql $ENV "select count(*) from users where name = 'Ada'")"
}

# treehash hashes the path and content of every file in a folder of the pool.
treehash() { in_daemon "cd '$1' && find . -type f -print0 | sort -z | xargs -0r sha256sum | sha256sum | cut -c1-16"; }

disk() {
  echo "== a big workspace"
  local D=/var/lib/fern/envs/big
  fern rm big 2>/dev/null || true
  fern create big >/dev/null
  # 1,000 folders of 100 small files, as node_modules has, and a 1 GB file.
  in_daemon "cd $D/live && mkdir -p node_modules/p0 && cd node_modules &&
    for f in \$(seq 100); do head -c 1024 /dev/urandom > p0/f\$f; done &&
    for d in \$(seq 999); do cp -r --reflink=never p0 p\$d; done &&
    head -c 1000000000 /dev/urandom > ../blob && chown -R 1000:1000 $D/live && sync"
  note "workspace: $(in_daemon "find $D/live | wc -l") files, $(in_daemon "du -sh $D/live | cut -f1")"
  for i in $(seq 10); do fern snapshot big "s$i" | took; done >"$TMP/snap"
  note "snapshot ms: $(stats <"$TMP/snap")"
  in_daemon "echo changed > $D/live/blob && rm $D/live/node_modules/p0/f1 && echo new > $D/live/new"
  for i in $(seq 5); do fern restore big "s$i" | took; done >"$TMP/restore"
  note "restore ms (asleep, so no box to stop): $(stats <"$TMP/restore")"
  check "a restore equals its snapshot (tree hash)" "$(treehash $D/snaps/s1)" "$(treehash $D/live)"
  start=$(now_ms)
  fern rm big
  note "rm with 10 snapshots: $(($(now_ms) - start)) ms"
  check "rm leaves nothing behind" no "$(succeeds in_daemon "test -e $D")"

  python3 -c '
import os, sys
for d in range(200):
    os.makedirs(f"{sys.argv[1]}/d{d}")
    for f in range(100):
        open(f"{sys.argv[1]}/d{d}/f{f}", "wb").write(os.urandom(1024))' "$TMP/repo"
  start=$(now_ms)
  fern create seeded --repo "$TMP/repo" >/dev/null
  note "create seeded from the Mac with 20,000 files (20 MB): $(($(now_ms) - start)) ms"
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

stream() {
  echo "== the exec stream"
  fresh $ENV
  fern exec $ENV -- true
  python3 -c 'import json; [print(json.dumps({"type": "user", "n": i, "text": "é☃" * 20})) for i in range(10000)]' >"$TMP/lines"
  check "10,000 JSON lines through cat, unchanged" "$(sha256 <"$TMP/lines")" "$(fern exec -i $ENV -- cat <"$TMP/lines" | sha256)"
  head -c 50000000 /dev/urandom >"$TMP/bin"
  start=$(now_ms)
  got=$(fern exec -i $ENV -- cat <"$TMP/bin" | sha256)
  note "50 MB round trip (Mac -> box -> Mac): $(($(now_ms) - start)) ms"
  check "50 MB of random bytes through cat, unchanged" "$(sha256 <"$TMP/bin")" "$got"
  # Counted on the node: kubectl exec, which carries a command's output to
  # the Mac, sometimes cuts a large one short over a slow link (the VPS).
  start=$(now_ms)
  n=$(in_daemon "fern exec $ENV -- head -c 200000000 /dev/zero | wc -c" | tr -d ' ')
  note "200 MB of stdout: $(($(now_ms) - start)) ms"
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
  note "10 parallel execs on a sleeping box: $(($(now_ms) - start)) ms in all"
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

# pg_load ENV N takes N snapshots of ENV's Postgres, one a second, while
# pgbench writes to it, then restores each and checks that Postgres starts,
# pg_amcheck finds no corruption, and every transaction is whole: pgbench
# moves each amount between an account, a teller and a branch, so their
# balances sum to the same.
pg_load() {
  local env=$1 n=$2 ok=0
  fern exec "$env" -- pgbench -iq -s 5 >/dev/null 2>&1
  fern exec "$env" -- pgbench -n -c 4 -T $((n + 10)) >/dev/null 2>&1 &
  local load=$!
  sleep 2
  for i in $(seq "$n"); do
    fern snapshot "$env" "p$i" | took >>"$TMP/pg-snap-$env"
    sleep 1
  done
  wait $load
  local whole="select (select sum(abalance) from pgbench_accounts) = (select sum(bbalance) from pgbench_branches)
    and (select sum(bbalance) from pgbench_branches) = (select sum(tbalance) from pgbench_tellers)"
  for i in $(seq "$n"); do
    fern restore "$env" "p$i" >/dev/null
    if [ "$(sql "$env" "$whole" 2>/dev/null)" = t ] &&
      fern exec "$env" -- pg_amcheck --install-missing --heapallindexed postgres >/dev/null 2>&1; then
      ok=$((ok + 1))
    else
      note "snapshot p$i: Postgres isn't consistent"
    fi
  done
  note "snapshot under load, ms: $(stats <"$TMP/pg-snap-$env")"
  check "Postgres is whole and uncorrupted after each of $n restores" "$n" "$ok"
}

pgload() {
  echo "== Postgres recovers from snapshots taken under load"
  fresh $ENV
  pg_load $ENV 60
}

# daemon_args sets the daemon's arguments, a JSON list, and waits for it.
daemon_args() {
  k -n fern-system patch ds fern --type=json \
    -p "[{\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/args\", \"value\": $1}]" >/dev/null
  wait_daemon
}

idle() {
  echo "== an idle box goes to sleep"
  # The deployed arguments, such as the runtime class, stay.
  local args
  args=$(k -n fern-system get ds fern -o jsonpath='{.spec.template.spec.containers[0].args}')
  daemon_args "${args%]}, \"--idle-agent=5s\", \"--idle-human=10s\"]"
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
  k -n fern-system delete pod -l app.kubernetes.io/name=fern --wait=false >/dev/null
  wait_daemon
  sleep 10
  check "a box awake across a daemon restart goes to sleep too" asleep "$(asleep_within $ENV 5)"
  daemon_args "$args"
}

daemon() {
  echo "== the daemon"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Grace', 'grace@example.com')" >/dev/null
  uid=$(pod_uid $ENV)
  start=$(now_ms)
  k -n fern-system delete pod -l app.kubernetes.io/name=fern --wait=false >/dev/null
  wait_daemon
  note "the daemon was back after $(($(now_ms) - start)) ms"
  check "the box kept running while the daemon restarted (same pod)" "$uid" "$(pod_uid $ENV)"
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
  note "10 sleeping environments woken at once: $(($(now_ms) - start)) ms until all answered"
  check "all 10 answer (3 users each)" 10 "$(cat "$TMP"/many* | grep -cx 3 || true)"
  check "all 10 run under gVisor" 10 \
    "$(k -n fern-boxes get pods -o jsonpath='{range .items[*]}{.metadata.name} {.spec.runtimeClassName}{"\n"}{end}' | grep -cE '^m[0-9]+ gvisor$' || true)"
  if [ -n "$FERN_HOST" ]; then
    note "node memory used with 10 boxes awake: $(on_node free -m </dev/null | awk 'NR == 2 { print $3 }') MiB"
  else
    note "lab memory with 10 boxes awake: $(docker stats --no-stream --format '{{.MemUsage}}' "$LAB")"
  fi
  for i in $(seq 10); do fern rm "m$i"; done
}

limits() {
  echo "== a box's limits"
  fresh $ENV
  fresh ${ENV}2
  # The runtime mounts the workspace without nosuid (whatever the CSI plugin
  # sets); no_new_privs is what makes setuid files useless in the box.
  check "commands run with no_new_privs" 1 "$(fern exec $ENV -- grep NoNewPrivs /proc/self/status | awk '{print $2}')"
  check "the box's pod has a CPU limit (2 cores)" "200000 100000" "$(pod_cgroup $ENV cpu.max)"
  check "the box's pod has a PID limit" 4096 "$(pod_cgroup $ENV pids.max)"
  fern wake ${ENV}2 >/dev/null
  ip=$(k -n fern-boxes get pod ${ENV}2 -o jsonpath='{.status.podIP}')
  check "a box can't reach another box" no "$(succeeds fern exec $ENV -- curl -s -m 3 "http://$ip:7070/")"
  check "the daemon can" ok "$(fern exec ${ENV}2 -- echo ok)"
  fern rm ${ENV}2
  ip=$(k -n fern-system get pod -l app.kubernetes.io/name=fern -o jsonpath='{.items[0].status.podIP}')
  check "a box can't reach the daemon's metrics" no "$(succeeds fern exec $ENV -- curl -s -m 3 "http://$ip:9090/metrics")"
  check "or the Kubernetes API" no "$(succeeds fern exec $ENV -- curl -sk -m 3 https://10.43.0.1/)"
  check "but reaches the internet" yes "$(succeeds fern exec $ENV -- curl -s -m 10 -o /dev/null https://api.anthropic.com/)"
  # A pod that isn't the environment's box can't mount its folders.
  k apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: fern-thief, namespace: default, labels: {app.kubernetes.io/part-of: fern}}
spec:
  containers:
    - {name: c, image: fern-box:dev, imagePullPolicy: Never, command: [sleep, "60"], volumeMounts: [{name: w, mountPath: /w}]}
  volumes: [{name: w, csi: {driver: fern.dev, volumeAttributes: {env: $ENV, folder: workspace}}}]
YAML
  sleep 10
  local refused=no
  k get events -n default --field-selector involvedObject.name=fern-thief | grep -q 'may not mount' && refused=yes
  check "another pod can't mount an environment's workspace" yes "$refused"
  k delete pod fern-thief --now >/dev/null
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
  start=$(now_ms)
  fern create adopted --path $W/adopted >/dev/null
  note "adopting the sample repo: $(($(now_ms) - start)) ms"
  check "the adopted workspace is a subvolume" 256 "$(in_daemon "stat -c %i $W/adopted")"
  check "the adopted workspace runs Postgres (3 users)" 3 "$(sql adopted 'select count(*) from users')"
  in_daemon "mkdir -p $W/big && cd $W/big && seq 20000 | xargs -n 2000 sh -c 'for i; do echo \$i > f\$i; done' _ && chown -R 1000:1000 $W/big"
  start=$(now_ms)
  fern create adopted-big --path $W/big >/dev/null
  note "adopting 20,000 files: $(($(now_ms) - start)) ms"
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

t3() {
  echo "== T3's server on Fern's node"
  if ! k -n fern-system rollout status deploy/t3 --timeout=180s >/dev/null 2>&1; then
    check "T3's server runs (lab/build.sh t3)" yes no
    return
  fi
  # A VPS's NodePort is reached through an SSH tunnel at T3_PORT.
  check "the Mac reaches T3 through the NodePort" 200 \
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
  start=$(now_ms)
  in_t3 "fern create --path $W t3c" >/dev/null
  note "T3's worktree adopted (first checkpoint): $(($(now_ms) - start)) ms"
  check "claude in a T3 worktree runs" "$CLAUDE" "$(in_t3 "cd $W && claude --version")"
  check "in its box, which it woke" running "$(state t3c)"
  # Claude's folder is outside the workspace, so sleep and restore keep it.
  local C=/var/lib/fern/envs/t3c/claude
  check "the box keeps Claude's state outside the workspace" "$C" \
    "$(in_t3 "cd $W && fern exec --here -- sh -c 'echo \$CLAUDE_CONFIG_DIR'")"
  in_t3 "cd $W && fern exec --here -- sh -c 'echo kept > $C/note'"
  check "Claude is told it is in a Fern environment" yes \
    "$(in_t3 "cd $W && fern exec --here -- cat $C/CLAUDE.md" | grep -q 'Fern environment' && echo yes || echo no)"
  check "git works in the box" init "$(in_t3 "cd $W && fern exec --here -- git log --format=%s -1")"
  check "and can't change the main repository" no "$(succeeds in_t3 "cd $W && fern exec --here -- touch $R/.git/x")"
  check "or point the worktree's .git elsewhere" no \
    "$(succeeds in_t3 "cd $W && fern exec --here -- sh -c 'echo gitdir: /tmp > .git'")"
  check "claude in a worktree without an environment doesn't run" no \
    "$(succeeds in_t3 "mkdir -p /var/lib/fern/worktrees/t3check/none && cd /var/lib/fern/worktrees/t3check/none && claude --version")"
  # A thread in T3's Local mode works in the main folder, which has no box.
  check "claude in a project's main folder answers T3's probes" "$CLAUDE" "$(in_t3 "cd $R && claude --version")"
  check "but a thread's session there is refused, naming New worktree" yes \
    "$(in_t3 "cd $R && claude --session-id 00000000-0000-4000-8000-000000000000 -p hi 2>&1" | grep -q 'New worktree' && echo yes || echo no)"
  check "T3's new threads are New worktrees, and deleting one removes it" "worktree true" \
    "$(in_t3 "node -p 'const s = require(\"/var/lib/fern/userdata/settings.json\"); s.defaultThreadEnvMode + \" \" + s.storageCleanup.worktreeOnDelete'")"
  in_t3 "echo v1 > $W/a.txt && echo junk > $W/ignored.bin"
  note "checkpoint snapshot: $(in_t3 'fern snapshot --replace t3c c1' | took) ms"
  in_t3 "echo v2 > $W/a.txt"
  note "checkpoint snapshot, replaced: $(in_t3 'fern snapshot --replace t3c c1' | took) ms"
  in_t3 "echo v3 > $W/a.txt && rm $W/ignored.bin"
  note "restore: $(in_t3 'fern restore t3c c1' | took) ms"
  check "restore brings back the replaced snapshot" v2 "$(in_t3 "cat $W/a.txt")"
  check "and a file git doesn't track" junk "$(in_t3 "cat $W/ignored.bin")"
  check "git works in the restored worktree" "M a.txt" "$(in_t3 "git -C $W status --short --untracked-files=no" | xargs)"
  check "Claude's state outlives the restore" kept "$(in_t3 "cd $W && fern exec --here -- cat $C/note")"
  # The login reaches the box from the Secret, which a box reads when it
  # starts. Without one, a fake one shows the way works: Claude then fails at
  # Anthropic's API instead of asking for a login.
  if k -n fern-boxes get secret claude-login >/dev/null 2>&1; then
    fern sleep t3c >/dev/null
    check "the box has Claude's login" yes \
      "$(succeeds in_t3 "cd $W && fern exec --here -- sh -c 'test -n \"\$CLAUDE_CODE_OAUTH_TOKEN\"'")"
    check "T3 has Claude's login" yes "$(succeeds in_t3 "test -n \"\$CLAUDE_CODE_OAUTH_TOKEN\"")"
  else
    printf %s fern-lab-not-a-token | k -n fern-boxes create secret generic claude-login --from-file=token=/dev/stdin >/dev/null
    fern sleep t3c >/dev/null
    check "Claude in the box uses the login from the Secret" "API Error: 401" \
      "$(in_t3 "cd $W && claude -p hi 2>&1" 2>/dev/null | grep -o 'API Error: 401' | head -1)"
    k -n fern-boxes delete secret claude-login >/dev/null
    note "no Claude login stored yet: lab/claude-login.sh"
  fi
  in_t3 "fern rm t3c && rm -rf $R /var/lib/fern/worktrees/t3check"
}

all=(slice box disk stream concurrency restore pgload idle daemon node many limits paths claude)
[ $# -gt 0 ] || set -- "${all[@]}"
for s in "$@"; do
  case " ${all[*]} t3 " in
    *" $s "*) ;;
    *) echo "no section $s; the sections are: ${all[*]} t3" >&2; exit 2 ;;
  esac
done
for s in "$@"; do "$s"; done
fern rm $ENV 2>/dev/null || true
if [ "$FAILED" = 0 ]; then echo "check: PASS"; else echo "check: FAIL"; exit 1; fi
