# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section many of lab/check.sh, which sources this file: it has no use by itself.
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
