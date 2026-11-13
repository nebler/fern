# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section idle_exec of lab/check.sh, which sources this file: it has no use by itself.
# idle_exec: a command that prints nothing for IDLE seconds (180) still
# answers: nothing between the client and the box drops an idle stream.
idle_exec() {
  echo "== an idle exec stream"
  fresh $ENV
  check "a command idle for ${IDLE}s still answers" alive "$(fern exec $ENV -- sh -c "sleep $IDLE; echo alive")"
}
