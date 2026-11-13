# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# What lab/check.sh's sections share: recording a check, timings, and reading
# the node. lab/check.sh sources this file; it has no use by itself.
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

# mounts counts environment $1's box's mounts on the node: its root's.
mounts() { on_node grep -c "/boxes/$1/rootfs " /proc/self/mountinfo </dev/null || true; }

# procs prints the names of the processes in an environment's box.
procs() { fern exec "$1" -- sh -c 'cat /proc/[0-9]*/comm 2>/dev/null; true' | sort -u; }

# treehash hashes the path and content of every file in a folder of the pool.
treehash() { in_daemon "cd '$1' && find . -type f -print0 | sort -z | xargs -0r sha256sum | sha256sum | cut -c1-16"; }

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
