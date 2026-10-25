#!/usr/bin/env bash
# Runs a demo scenario through T3's server many times, as the T3 app would,
# and times every step: lab/t3-run.sh [toy|sample] [RUNS] [MODEL]. Each run
# starts a New worktree thread, sends the scenario's messages, reverts a
# turn and checks the answers; lab/t3-run.mjs has the scenarios. Results go
# to lab/t3-runs.jsonl, one line per run. KEEP=1 leaves the threads in T3;
# PARALLEL=N runs N threads at once and reports the node's peak memory.
#
# Needs lab/t3-pair.sh once (it adds the projects). With FERN_HOST it
# reaches the VPS's T3 through an SSH tunnel of its own. The model is the
# user's subscription, so the default is the cheapest one.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
port=30773
if [ -n "$FERN_HOST" ]; then
  # A port of its own, so the T3 app's tunnel (30774) can stay open.
  port=30775
  "${SSH[@]}" -o ExitOnForwardFailure=yes -N -L "127.0.0.1:$port:127.0.0.1:30773" "root@$FERN_HOST" &
  tunnel=$!
  trap 'kill $tunnel 2>/dev/null' EXIT
  for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/" && break; sleep 0.2; done
fi
pair_url=$(k -n fern-system exec deploy/t3 -c t3 -- node /opt/t3/apps/server/dist/bin.mjs \
  pair --ttl 5m --label "t3-run on $(hostname -s)" </dev/null |
  sed -nE "s#^Pairing URL: http://[^/]+/#http://127.0.0.1:$port/#p")
# The driver reads the daemon's metrics before and after every step,
# shellcheck disable=SC2016 # expanded later, by the driver's bash -c
metrics='source lab/lib.sh
pod=$(k -n fern-system get pod -l app.kubernetes.io/name=fern -o jsonpath="{.items[0].metadata.name}")
k get --raw "/api/v1/namespaces/fern-system/pods/$pod:9090/proxy/metrics"'
# deletes each run's environment, given its worktree's path,
# shellcheck disable=SC2016
rm_env='source lab/lib.sh; id=$(fern which "$1") && fern rm "$id"'
# and samples the node's used memory, in MiB.
# shellcheck disable=SC2016
node_memory='source lab/lib.sh; on_node free -m </dev/null | awk "NR == 2 { print \$3 }"'
T3_PAIR_URL=$pair_url T3_METRICS_CMD=$metrics T3_RM_ENV_CMD=$rm_env T3_NODE_MEMORY_CMD=$node_memory \
  FERN_HOST=$FERN_HOST SCENARIO=${1:-toy} RUNS=${2:-1} MODEL=${3:-claude-haiku-5-5} PARALLEL=${PARALLEL:-1} \
  node lab/t3-run.mjs
