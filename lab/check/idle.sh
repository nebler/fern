# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section idle of lab/check.sh, which sources this file: it has no use by itself.
idle() {
  echo "== an idle box goes to sleep"
  # The deployed arguments, such as --agent-boxes, stay.
  local args
  args=$(daemon_args)
  set_daemon_args $args --idle-agent=5s --idle-human=10s
  fresh $ENV
  fern exec $ENV -- true
  check "a box is awake after a command" running "$(state $ENV)"
  sleep 10
  check "and asleep 10 s after the command started" asleep "$(asleep_within $ENV 5)"
  fern exec $ENV -- sleep 20 &
  local open=$!
  sleep 15
  check "a running command keeps it awake past the idle times" running "$(state $ENV)"
  wait $open
  sleep 5
  check "it sleeps 5 s after the command ended" asleep "$(asleep_within $ENV 5)"
  fern wake $ENV >/dev/null
  restart_daemon
  sleep 10
  check "a box awake across a daemon restart goes to sleep too" asleep "$(asleep_within $ENV 5)"
  # Used at 0, the daemon restarting at 7: its clocks go on from 0, so it
  # sleeps by about 10, not at 17.
  fern exec $ENV -- true
  sleep 7
  restart_daemon
  check "a daemon restart doesn't start the idle clocks over" asleep "$(asleep_within $ENV 5)"
  set_daemon_args $args
}
