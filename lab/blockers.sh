#!/usr/bin/env bash
# Checks the risks that could block the MVP, as far as the lab can show them
# without logging in to anything:
#   claude       Claude Code starts in a box, reaches the network, and speaks
#                stream-json through fern exec (without a login)
#   stream       fern exec keeps a stream intact: bytes, stderr, exit codes,
#                stdin EOF, Ctrl-C, long idle commands, cancellation
#   concurrency  parallel execs on an awake and on a sleeping environment
#   restore      a restore while an exec is open
#   daemon       the daemon restarts while a box is awake
#   node         the whole node (k3s) restarts
#   bigrepo      a 100,000-file workspace: snapshot, restore, rm; a real clone
#   many         ten environments woken at once
#   gvisor       Claude Code and the exec stream under gVisor (installs it)
#   review       the fixes from the code review of 2026-10-21: stdin EOF,
#                limits, mount flags, network rules, restore and rm safety
# Run all of them, or name some: lab/blockers.sh stream daemon. Needs lab/up.sh.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."
ENV=bk IDLE=${IDLE:-180}

took() { sed -nE 's/.* took ([0-9.]+)ms$/\1/p'; }
sql() { fern exec "$1" -- psql -tAc "$2"; }
note() { echo "NOTE $*"; }
# hash prints the SHA-256 of stdin.
hash() { shasum -a 256 | cut -d' ' -f1; }
# pod_uid prints the UID of an environment's pod, or nothing.
pod_uid() { k -n fern-boxes get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null || true; }
wait_daemon() {
  k -n fern-system rollout status ds/fern --timeout=300s >/dev/null
  until k -n fern-system exec ds/fern -c fern -- fern ls >/dev/null 2>&1; do sleep 1; done
}
fresh() { fern rm "$1" 2>/dev/null || true; fern create "$1" --repo lab/sample-repo >/dev/null; }

claude() {
  echo "== Claude Code in a box (no login)"
  fresh $ENV
  check "claude runs (read-only root, uid 1000)" "2.1.304 (Claude Code)" "$(fern exec $ENV -- claude --version)"
  code=$(fern exec $ENV -- curl -s -o /dev/null -w '%{http_code}' https://api.anthropic.com/ || true)
  check "the box reaches api.anthropic.com (any HTTP status)" yes "$([ "${code:-000}" != 000 ] && echo yes || echo "no ($code)")"
  out=$(printf '%s\n' '{"type":"user","message":{"role":"user","content":"say hi"}}' |
    fern exec -i $ENV -- claude -p --input-format stream-json --output-format stream-json --verbose 2>/tmp/fern-bk-claude-err || true)
  echo "$out" > /tmp/fern-bk-claude-out
  bad=$(echo "$out" | python3 -c 'import json,sys
n=0
for l in sys.stdin:
    if l.strip():
        try: json.loads(l)
        except Exception: n+=1
print(n)')
  check "stream-json output arrives as whole JSON lines" 0 "$bad"
  note "stream-json without a login: $(echo "$out" | grep -c . || true) lines;" \
    "types: $(echo "$out" | python3 -c 'import json,sys; print(sorted({json.loads(l).get("type","?")+"/"+str(json.loads(l).get("subtype","")) for l in sys.stdin if l.strip()}))' 2>/dev/null)"
  note "result: $(echo "$out" | python3 -c 'import json,sys
for l in sys.stdin:
    if l.strip():
        m=json.loads(l)
        if m.get("type")=="result": print(str(m.get("result") or m.get("errors") or m)[:160])' 2>/dev/null)"
  note "Claude keeps its state in: $(fern exec $ENV -- sh -c 'ls -d $HOME/.claude* 2>/dev/null | tr "\n" " "') (HOME=/tmp, an emptyDir: a login is lost on every sleep)"
  # A terminal, as T3's terminal or "fern exec -it" would give it.
  tui=$(in_daemon "(sleep 6; printf '\003'; sleep 1; printf '\003'; sleep 5) | timeout 30 script -qec 'fern exec -it $ENV -- claude' /dev/null 2>&1 | tr -d '\033' | tr -c '[:print:]\n' ' ' | tail -c 2000" || true)
  echo "$tui" > /tmp/fern-bk-claude-tui
  check "interactive claude draws its screen through a PTY" yes "$(echo "$tui" | grep -qiE 'claude|login|theme|welcome' && echo yes || echo no)"
  check "two Ctrl-Cs quit interactive claude" no "$(echo "$tui" | grep -q 'context canceled' && echo yes || echo no)"
}

stream() {
  echo "== the exec stream"
  fresh $ENV
  fern exec $ENV -- true
  python3 -c 'import json; [print(json.dumps({"type":"user","n":i,"text":"é☃"*20})) for i in range(10000)]' > /tmp/fern-bk-lines
  check "10,000 JSON lines through cat, unchanged" "$(hash < /tmp/fern-bk-lines)" "$(fern exec -i $ENV -- cat < /tmp/fern-bk-lines | hash)"
  head -c 50000000 /dev/urandom > /tmp/fern-bk-bin
  start=$(now_ms)
  got=$(fern exec -i $ENV -- cat < /tmp/fern-bk-bin | hash)
  ms=$(( $(now_ms) - start ))
  check "50 MB of random bytes through cat, unchanged" "$(hash < /tmp/fern-bk-bin)" "$got"
  note "50 MB round trip (Mac -> box -> Mac): $ms ms"
  start=$(now_ms)
  n=$(fern exec $ENV -- head -c 200000000 /dev/zero | wc -c | tr -d ' ')
  note "200 MB of stdout: $(( $(now_ms) - start )) ms"
  check "200 MB of stdout, every byte" 200000000 "$n"
  check "stdout and stderr stay apart" out "$(fern exec $ENV -- sh -c 'echo out; echo err >&2' 2>/dev/null)"
  check "exit code passes through" 42 "$(fern exec $ENV -- sh -c 'exit 42' >/dev/null 2>&1; echo $?)"
  check "stdin EOF reaches the command" 3 "$(printf 'a\nb\nc\n' | fern exec -i $ENV -- wc -l | tr -d ' ')"
  rc=$(in_daemon "(sleep 2; printf '\003') | script -qec 'fern exec -it $ENV -- sleep 30' /dev/null >/dev/null 2>&1; echo \$?" || true)
  check "Ctrl-C in a terminal stops the command (130)" 130 "$rc"
  in_daemon "timeout 3 fern exec $ENV -- sleep 301 || true"
  sleep 2
  check "a client that goes away stops its command" 0 "$(fern exec $ENV -- sh -c 'cat /proc/[0-9]*/cmdline 2>/dev/null | tr "\0" " "' | grep -c 'sleep 301' || true)"
  check "a command idle for ${IDLE}s still answers" alive "$(fern exec $ENV -- sh -c "sleep $IDLE; echo alive")"
}

concurrency() {
  echo "== concurrency"
  fresh $ENV
  fern exec $ENV -- true
  for i in $(seq 10); do (fern exec $ENV -- echo "ok$i" > "/tmp/fern-bk-par$i" 2>&1) & done
  wait
  check "10 parallel execs on an awake box" 10 "$(cat /tmp/fern-bk-par* | grep -c '^ok' || true)"
  rm -f /tmp/fern-bk-par*
  fern sleep $ENV >/dev/null
  start=$(now_ms)
  for i in $(seq 10); do (fern exec $ENV -- psql -tAc 'select count(*) from users' > "/tmp/fern-bk-par$i" 2>&1) & done
  wait
  note "10 parallel execs on a sleeping box: $(( $(now_ms) - start )) ms in total"
  check "all 10 see the database (one wake)" 10 "$(cat /tmp/fern-bk-par* | grep -c '^3$' || true)"
  rm -f /tmp/fern-bk-par*
}

restore() {
  echo "== restore while an exec is open"
  fresh $ENV
  fern exec $ENV -- true
  fern snapshot $ENV r1 >/dev/null
  start=$(now_ms)
  (fern exec $ENV -- sh -c 'sleep 120; echo finished' > /tmp/fern-bk-open 2>&1; echo "exit=$?" >> /tmp/fern-bk-open) &
  sleep 2
  fern restore $ENV r1 >/dev/null
  wait
  note "the open exec ended after $(( $(now_ms) - start )) ms with: $(tr '\n' ' ' < /tmp/fern-bk-open)"
  check "restore cuts the open exec (no hang, not 'finished')" 0 "$(grep -c finished /tmp/fern-bk-open || true)"
  check "the environment works after it (3 users)" 3 "$(sql $ENV 'select count(*) from users')"
}

daemon() {
  echo "== the daemon restarts while a box is awake"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Grace', 'grace@example.com')" >/dev/null
  uid=$(pod_uid $ENV)
  start=$(now_ms)
  k -n fern-system delete pod -l app.kubernetes.io/name=fern --wait=false >/dev/null
  wait_daemon
  note "daemon back in $(( $(now_ms) - start )) ms"
  check "the box kept running (same pod)" "$uid" "$(pod_uid $ENV)"
  check "Postgres kept its data (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
  check "the box still sees live/ (seed.sql)" yes "$(fern exec $ENV -- sh -c 'test -s seed.sql && echo yes')"
  fern snapshot $ENV d1 >/dev/null
  fern restore $ENV d1 >/dev/null
  check "snapshot and restore work after the restart" 4 "$(sql $ENV 'select count(*) from users')"
}

node() {
  echo "== the node restarts"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Alan', 'alan@example.com')" >/dev/null
  fern snapshot $ENV n1 >/dev/null
  sql $ENV "insert into users (name, email) values ('Katherine', 'katherine@example.com')" >/dev/null
  start=$(now_ms)
  docker restart "$LAB" >/dev/null
  until k get nodes 2>/dev/null | grep -q ' Ready'; do sleep 1; done
  wait_daemon
  note "node and daemon back in $(( $(now_ms) - start )) ms"
  check "the environment is still listed" "$ENV" "$(fern ls | awk -v e=$ENV '$1 == e {print $1}')"
  start=$(now_ms)
  check "the box comes back with its data (5 users)" 5 "$(sql $ENV 'select count(*) from users')"
  note "first exec after the restart: $(( $(now_ms) - start )) ms"
  fern restore $ENV n1 >/dev/null
  check "a snapshot from before the restart restores (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
}

bigrepo() {
  echo "== a big workspace"
  fern rm big 2>/dev/null || true
  fern create big >/dev/null
  in_daemon 'cd /var/lib/fern/envs/big/live && mkdir -p node_modules && cd node_modules &&
    mkdir p0 && for f in $(seq 100); do head -c 1024 /dev/urandom > p0/f$f; done &&
    for d in $(seq 999); do cp -r --reflink=never p0 p$d; done &&
    head -c 1000000000 /dev/urandom > ../blob && chown -R 1000:1000 /var/lib/fern/envs/big/live && sync'
  note "workspace: $(in_daemon 'find /var/lib/fern/envs/big/live | wc -l') files, $(in_daemon 'du -sh /var/lib/fern/envs/big/live | cut -f1')"
  : > /tmp/fern-bk-snap
  for i in $(seq 10); do fern snapshot big "s$i" | took >> /tmp/fern-bk-snap; done
  note "snapshot ms: $(stats < /tmp/fern-bk-snap)"
  : > /tmp/fern-bk-restore
  for i in $(seq 5); do fern restore big "s$i" | took >> /tmp/fern-bk-restore; done
  note "restore ms (asleep, so no box to stop): $(stats < /tmp/fern-bk-restore)"
  start=$(now_ms)
  fern rm big
  note "rm with 10 snapshots: $(( $(now_ms) - start )) ms"
  check "nothing left of it" no "$(in_daemon 'test -e /var/lib/fern/envs/big && echo yes || echo no')"

  # Today's create copies the repo as a tar; golden snapshots replace this.
  rm -rf /tmp/fern-bk-repo && mkdir -p /tmp/fern-bk-repo
  python3 -c '
import os
for d in range(200):
    os.makedirs(f"/tmp/fern-bk-repo/d{d}")
    for f in range(100):
        open(f"/tmp/fern-bk-repo/d{d}/f{f}", "wb").write(os.urandom(1024))'
  start=$(now_ms)
  fern create tarred --repo /tmp/fern-bk-repo >/dev/null
  note "create --repo with 20,000 files (20 MB) from the Mac: $(( $(now_ms) - start )) ms"
  fern rm tarred
  rm -rf /tmp/fern-bk-repo

  fresh $ENV
  start=$(now_ms)
  if fern exec $ENV -- git clone -q --depth 1 https://github.com/pingdotgg/t3code.git t3code; then
    note "git clone --depth 1 of t3code inside the box: $(( $(now_ms) - start )) ms," \
      "$(fern exec $ENV -- sh -c 'find t3code | wc -l') files, $(fern exec $ENV -- du -sh t3code | cut -f1)"
    note "snapshot with it: $(fern snapshot $ENV c1 | took) ms"
  else
    note "git clone of t3code failed"
  fi
}

many() {
  echo "== ten environments"
  for i in $(seq 10); do fresh "m$i"; done
  start=$(now_ms)
  for i in $(seq 10); do (sql "m$i" 'select count(*) from users' > "/tmp/fern-bk-m$i" 2>&1) & done
  wait
  note "10 sleeping environments woken at once: $(( $(now_ms) - start )) ms until all answered"
  check "all 10 answer (3 users each)" 10 "$(cat /tmp/fern-bk-m* | grep -c '^3$' || true)"
  note "lab memory with 10 awake boxes: $(docker stats --no-stream --format '{{.MemUsage}}' "$LAB")"
  rm -f /tmp/fern-bk-m*
  for i in $(seq 10); do fern rm "m$i"; done
}

gvisor() {
  echo "== under gVisor"
  RUNS=1 lab/gvisor.sh >/dev/null # installs gVisor and the RuntimeClass if needed
  fresh gk
  k -n fern-boxes patch sandbox gk --type=merge -p '{"spec":{"podTemplate":{"spec":{"runtimeClassName":"gvisor"}}}}' >/dev/null
  check "the box runs under gVisor" "Starting gVisor..." "$(fern exec gk -- dmesg | head -1 | cut -c16-)"
  check "claude runs under gVisor" "2.1.304 (Claude Code)" "$(fern exec gk -- claude --version)"
  code=$(fern exec gk -- curl -s -o /dev/null -w '%{http_code}' https://api.anthropic.com/ || true)
  check "network under gVisor (any HTTP status)" yes "$([ "${code:-000}" != 000 ] && echo yes || echo "no ($code)")"
  head -c 20000000 /dev/urandom > /tmp/fern-bk-bin
  check "20 MB through cat under gVisor, unchanged" "$(hash < /tmp/fern-bk-bin)" "$(fern exec -i gk -- cat < /tmp/fern-bk-bin | hash)"
  fern rm gk
}

review() {
  echo "== fixes from the code review"
  fresh $ENV
  fresh bk2
  check "exec without -i sees stdin EOF" ok \
    "$(in_daemon "timeout 20 fern exec $ENV -- sh -c 'cat >/dev/null && echo ok'" || echo hang)"
  mounts=$(fern exec $ENV -- sh -c 'grep " /workspace " /proc/mounts')
  check "the workspace is mounted nosuid,nodev" yes \
    "$(echo "$mounts" | grep -q nosuid && echo "$mounts" | grep -q nodev && echo yes || echo "no: $mounts")"
  check "the box has a CPU limit (2 cores)" "200000 100000" "$(fern exec $ENV -- cat /sys/fs/cgroup/cpu.max)"
  check "the box has a PID limit" 4096 "$(fern exec $ENV -- cat /sys/fs/cgroup/pids.max)"
  fern wake bk2 >/dev/null
  ip=$(k -n fern-boxes get pod bk2 -o jsonpath='{.status.podIP}')
  code=$(fern exec $ENV -- curl -s -m 3 -o /dev/null -w '%{http_code}' "http://$ip:7070/healthz" || true)
  check "a box can't reach another box" 000 "${code:-000}"
  check "the daemon still reaches the other box" ok "$(fern exec bk2 -- echo ok)"
  if fern restore $ENV no-such-snapshot >/dev/null 2>&1; then r=restored; else r=refused; fi
  check "restoring a missing snapshot is refused" refused "$r"
  check "and leaves the box running" running "$(fern ls | awk -v e=$ENV '$1 == e { print $2 }')"
  note "sleep with Postgres (stop command, then SIGTERM): $(fern sleep $ENV | took) ms"
  # Ctrl-C a delete while it waits for the pod: the daemon must finish it.
  in_daemon "timeout 1 fern rm bk2" >/dev/null 2>&1 || true
  for _ in $(seq 60); do fern ls | grep -q '^bk2 ' || break; sleep 1; done
  check "an interrupted rm still finishes" gone "$(fern ls | grep -q '^bk2 ' && echo listed || echo gone)"
  check "and leaves no disk behind" gone "$(in_daemon 'ls /var/lib/fern/envs' | grep -qx bk2 && echo left || echo gone)"
}

sections=("$@")
[ ${#sections[@]} -eq 0 ] && sections=(claude stream concurrency restore daemon node bigrepo many gvisor review)
for s in "${sections[@]}"; do "$s"; done
fern rm $ENV 2>/dev/null || true
rm -f /tmp/fern-bk-*
if [ "$FAILED" = 0 ]; then echo "blockers: PASS"; else echo "blockers: FAIL"; exit 1; fi
