# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section stability of lab/check.sh, which sources this file: it has no use by itself.
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
