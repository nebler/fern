# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section concurrency of lab/check.sh, which sources this file: it has no use by itself.
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
