# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section limits of lab/check.sh, which sources this file: it has no use by itself.
limits() {
  echo "== a box's limits"
  fresh $ENV
  fresh ${ENV}2
  # The runtime mounts the workspace without nosuid; no_new_privs is what
  # makes setuid files useless in the box.
  check "commands run with no_new_privs" 1 "$(fern exec $ENV -- grep NoNewPrivs /proc/self/status | awk '{print $2}')"
  check "the box has a CPU limit (2 cores)" "200000 100000" "$(pod_cgroup $ENV cpu.max)"
  check "the box has a PID limit" 4096 "$(pod_cgroup $ENV pids.max)"
  # All boxes together: the machine's memory less a reserve for the node,
  # so that boxes alone can't take the daemon or T3 down with them.
  local total limit want
  total=$(on_node awk '/^MemTotal/ { print $2 * 1024 }' /proc/meminfo </dev/null)
  limit=$(on_node cat /sys/fs/cgroup/fern/memory.max </dev/null)
  want=$((total - 1610612736 > total / 2 ? total - 1610612736 : total / 2))
  check "the boxes together have a memory limit" "$want" "$limit"
  # The kernel keeps it in whole pages.
  check "… and are slowed a tenth before it" "$(((limit - limit / 10) / 4096 * 4096))" "$(on_node cat /sys/fs/cgroup/fern/memory.high </dev/null)"
  check "doctor doesn't list memory" 0 "$(fern doctor 2>/dev/null | grep -c '^FAIL  memory' || true)"
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
