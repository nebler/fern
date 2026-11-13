# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section daemon of lab/check.sh, which sources this file: it has no use by itself.
daemon() {
  echo "== the daemon"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Grace', 'grace@example.com')" >/dev/null
  uid=$(pod_uid $ENV)
  start=$(now_ms)
  restart_daemon
  note "the daemon was back after $(($(now_ms) - start)) ms"
  check "the box kept running while the daemon restarted (same sandbox)" "$uid" "$(pod_uid $ENV)"
  check "Postgres kept its data (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
  check "the box still sees its workspace" yes "$(succeeds fern exec $ENV -- test -s seed.sql)"
  fern snapshot $ENV d1 >/dev/null
  fern restore $ENV d1 >/dev/null
  check "snapshot and restore work after the restart" 4 "$(sql $ENV 'select count(*) from users')"
  # Ctrl-C an rm while it waits for the box to stop: the daemon finishes it.
  in_daemon "timeout 1 fern rm $ENV" >/dev/null 2>&1 || true
  for _ in $(seq 60); do [ -n "$(state $ENV)" ] || break; sleep 1; done
  check "an rm whose client went away still finishes" "" "$(state $ENV)"
  check "and leaves no disk behind" no "$(succeeds in_daemon "test -e /var/lib/fern/envs/$ENV")"
}
