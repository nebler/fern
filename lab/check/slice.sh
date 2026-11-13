# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section slice of lab/check.sh, which sources this file: it has no use by itself.
slice() {
  echo "== the whole disk, database included, is snapshotted and restored"
  fresh $ENV
  check "a new environment is asleep" asleep "$(state $ENV)"
  check "the first exec wakes its box (3 users)" 3 "$(sql $ENV 'select count(*) from users')"
  fern snapshot $ENV s1 >/dev/null
  fern exec $ENV -- psql -qc 'drop table users'
  check "the table is gone" 0 "$(sql $ENV "select count(*) from pg_tables where tablename = 'users'")"
  fern restore $ENV s1 >/dev/null
  check "a restore leaves it asleep" asleep "$(state $ENV)"
  check "the next exec wakes it with the table (3 users)" 3 "$(sql $ENV 'select count(*) from users')"
  local pre
  pre=$(fern snapshots $ENV | awk '$1 ~ /^pre-restore-/ { print $1 }')
  check "a restore keeps the workspace it replaced" 1 "$(grep -c . <<<"$pre")"
  fern restore $ENV "$pre" >/dev/null
  check "restoring that undoes the restore (no table)" 0 \
    "$(sql $ENV "select count(*) from pg_tables where tablename = 'users'")"
  fern rm $ENV
  check "rm removes it" "" "$(state $ENV)"
}
