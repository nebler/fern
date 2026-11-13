# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section pgload of lab/check.sh, which sources this file: it has no use by itself.
pgload() {
  echo "== Postgres recovers from snapshots taken under load"
  fresh $ENV
  pg_load $ENV 60
}
