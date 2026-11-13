# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section quota of lab/check.sh, which sources this file: it has no use by itself.
# quota checks a disk limit per environment (--env-disk-limit): a write in
# its box past the limit fails, fern doctor, fern df and the thread's Claude
# say so, a snapshot still works, and fern limit raising it lets the write
# through.
quota() {
  echo "== a disk limit per environment"
  # The daemon turns btrfs's simple quotas on at its start with the flag,
  # and they stay on: on a VPS, the server's pool, whose cost is still to
  # measure (docs/briefs/disk-limit-per-environment.md). Not there unless
  # they are on already, or QUOTA_ON_VPS=1.
  if [ -n "$FERN_HOST" ] && [ "${QUOTA_ON_VPS:-0}" != 1 ] &&
    ! in_daemon "btrfs qgroup show /var/lib/fern" >/dev/null 2>&1; then
    note "quota skipped: it would turn btrfs quotas on for good on $FERN_HOST's pool (QUOTA_ON_VPS=1 to run it)"
    return
  fi
  # The deployed arguments, such as --agent-boxes, stay. Every environment
  # on the node is brought in with the flag's limit, so it is one nothing
  # reaches; this section's own is set with fern limit.
  local args status
  args=$(daemon_args)
  set_daemon_args $args --env-disk-limit=1T
  fresh $ENV
  check "fern limit sets the environment's limit" yes "$(succeeds fern limit $ENV 64M)"
  # Random bytes, which btrfs can't compress below what they are.
  fern exec $ENV -- sh -c 'head -c 134217728 /dev/urandom > fill' >"$TMP/fill" 2>&1 && status=0 || status=$?
  check "a write past the limit fails in the box" yes "$([ "$status" != 0 ] && echo yes || echo no)"
  check "with \"Disk quota exceeded\"" yes "$(succeeds grep -q 'Disk quota exceeded' "$TMP/fill")"
  # The group counts what a transaction committed.
  in_daemon "btrfs filesystem sync /var/lib/fern" >/dev/null
  check "fern doctor lists it at its limit" 1 "$(fern doctor 2>/dev/null | grep -c "^FAIL  disk limit  *$ENV has reached its disk limit" || true)"
  fern df >"$TMP/df"
  check "fern df says it is at the limit" yes "$(succeeds grep -q "^environment $ENV .*AT THE LIMIT" "$TMP/df")"
  check "a snapshot still works at the limit" yes "$(succeeds fern snapshot $ENV full)"
  if [ "${AGENTS:-0}" = 1 ]; then
    fern sleep $ENV >/dev/null
    check "the thread's Claude is told at its box's next start" yes \
      "$(fern exec --agent $ENV -- cat /var/lib/fern/envs/$ENV/claude/CLAUDE.md | tr '\n' ' ' | grep -q 'reached its disk limit' && echo yes || echo no)"
  fi
  check "fern limit raises it" yes "$(succeeds fern limit $ENV 1G)"
  check "and the write then succeeds" ok "$(fern exec $ENV -- sh -c 'rm -f fill && head -c 134217728 /dev/urandom > fill && echo ok' 2>&1)"
  in_daemon "btrfs filesystem sync /var/lib/fern" >/dev/null
  check "fern doctor no longer lists it" 0 "$(fern doctor 2>/dev/null | grep -c "^FAIL  disk limit  *$ENV has" || true)"
  set_daemon_args $args
}
