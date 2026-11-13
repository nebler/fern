# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section restore of lab/check.sh, which sources this file: it has no use by itself.
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
