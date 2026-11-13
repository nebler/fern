# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section scale of lab/check.sh, which sources this file: it has no use by itself.
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
