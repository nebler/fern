# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section turn of lab/check.sh, which sources this file: it has no use by itself.
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
