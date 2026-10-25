#!/usr/bin/env bash
# Runs a demo scenario through T3's server many times, as the T3 app would,
# and times every step: lab/t3-run.sh [toy|sample] [RUNS] [MODEL]. Each run
# starts a New worktree thread, sends the scenario's messages, reverts a
# turn and checks the answers; lab/t3-run.mjs has the scenarios. Results go
# to lab/t3-runs.jsonl, one line per run. KEEP=1 leaves the threads in T3.
#
# Needs lab/t3-pair.sh once (it adds the projects) and the T3 app's tunnel
# for a VPS (FERN_HOST, T3_PORT=30774). The model is the user's subscription,
# so the default is the cheapest one.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
pair_url=$(k -n fern-system exec deploy/t3 -c t3 -- node /opt/t3/apps/server/dist/bin.mjs \
  pair --ttl 5m --label "t3-run on $(hostname -s)" </dev/null |
  sed -nE "s#^Pairing URL: http://[^/]+/#http://127.0.0.1:${T3_PORT:-30773}/#p")
# The driver reads the daemon's metrics before and after every step.
# shellcheck disable=SC2016 # expanded later, by the driver's bash -c
metrics='source lab/lib.sh
pod=$(k -n fern-system get pod -l app.kubernetes.io/name=fern -o jsonpath="{.items[0].metadata.name}")
k get --raw "/api/v1/namespaces/fern-system/pods/$pod:9090/proxy/metrics"'
# ... and deletes each run's environment, given its worktree's path.
# shellcheck disable=SC2016
rm_env='source lab/lib.sh; id=$(fern which "$1") && fern rm "$id"'
T3_PAIR_URL=$pair_url T3_METRICS_CMD=$metrics T3_RM_ENV_CMD=$rm_env FERN_HOST=$FERN_HOST \
  SCENARIO=${1:-toy} RUNS=${2:-1} MODEL=${3:-claude-haiku-5-5} \
  node lab/t3-run.mjs
