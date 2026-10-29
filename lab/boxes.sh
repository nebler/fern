#!/usr/bin/env bash
# Switches how the daemon runs the lab's boxes: lab/boxes.sh runsc | agents.
# agents, the default, runs Claude in a box of its own per environment
# (docs/briefs/claude-outside-the-box.md); runsc runs it in the
# environment's box. Awake boxes are put to sleep first.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
mode=${1:?usage: lab/boxes.sh runsc|agents}
case $mode in runsc | agents) ;; *) echo "boxes: runsc or agents" >&2 && exit 2 ;; esac
wait_daemon
fern ls | awk 'NR > 1 && $2 != "asleep" { print $1 }' | while read -r id; do fern sleep "$id" </dev/null >/dev/null; done
args=$(daemon_args | tr ' ' '\n' | grep -v -- '^--agent-boxes' | xargs)
[ "$mode" = runsc ] && args+=" --agent-boxes=false"
set_daemon_args $args
echo "boxes: $mode"
