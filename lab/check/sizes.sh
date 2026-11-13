# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section sizes of lab/check.sh, which sources this file: it has no use by itself.
sizes() {
  echo "== the daemon's flags set a box's size"
  local args
  args=$(daemon_args)
  set_daemon_args $args --box-cpus=1.5 --box-memory=512 --box-pids=300
  fresh $ENV
  fern wake $ENV >/dev/null
  check "a box gets the CPUs asked for" "150000 100000" "$(pod_cgroup $ENV cpu.max)"
  check "and the memory" 536870912 "$(pod_cgroup $ENV memory.max)"
  check "and the processes" 300 "$(pod_cgroup $ENV pids.max)"
  if [ "$AGENTS" = 1 ]; then
    fern exec --agent $ENV -- sleep 6 &
    local open=$!
    sleep 3
    check "an agent box keeps its own size" 4096 "$(pod_cgroup $ENV.agent pids.max)"
    wait $open
  fi
  # A box's size is set when it wakes: one awake across the change keeps its own.
  fern wake $ENV >/dev/null
  set_daemon_args $args
  check "an awake box keeps its size when the flags change" 300 "$(pod_cgroup $ENV pids.max)"
  fern sleep $ENV >/dev/null
  fern wake $ENV >/dev/null
  check "and gets the new one at its next wake" 4096 "$(pod_cgroup $ENV pids.max)"
  # A repository asks for its own size in services.yaml, which its agent can
  # write: without --box-max-…, only for less.
  fern exec $ENV -- sh -c 'printf "limits:\n  memory: 512\n  pids: 100000\n" >>services.yaml'
  fern sleep $ENV >/dev/null
  fern wake $ENV >/dev/null
  check "a repository gets the memory it asks for, if less" 536870912 "$(pod_cgroup $ENV memory.max)"
  check "but no more processes than the node gives" 4096 "$(pod_cgroup $ENV pids.max)"
  set_daemon_args $args --box-max-pids=5000 --box-max-memory=4096
  fern exec $ENV -- sh -c 'sed -i "s/memory: 512/memory: 3072/" services.yaml'
  fern sleep $ENV >/dev/null
  fern wake $ENV >/dev/null
  check "with a higher most, it gets more memory" 3221225472 "$(pod_cgroup $ENV memory.max)"
  check "and processes up to the most" 5000 "$(pod_cgroup $ENV pids.max)"
  set_daemon_args $args
}
