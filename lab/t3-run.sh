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
source lab/hardware.sh
hardware_uids
export HW_DAEMON HW_T3 HW_CONTROLLER
port=30773
if [ -n "$FERN_HOST" ]; then
  # A port of its own, so the T3 app's tunnel (30774) can stay open.
  port=30775
  "${SSH[@]}" -o ExitOnForwardFailure=yes -N -L "127.0.0.1:$port:127.0.0.1:30773" "root@$FERN_HOST" &
  tunnel=$!
  trap 'kill $tunnel 2>/dev/null || true' EXIT
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
# puts it to sleep, as idle time would, for a reopen step,
# shellcheck disable=SC2016
sleep_env='source lab/lib.sh; id=$(fern which "$1") && fern sleep "$id" >/dev/null'
# prints its box's state and the fern exec processes standing in for Claude
# in T3's pod, for an idle step,
# shellcheck disable=SC2016
idle_env='source lab/lib.sh; id=$(fern which "$1") && fern ls | awk -v id="$id" '\''$1 == id { print $2 }'\''
k -n fern-system exec deploy/t3 -c t3 -- sh -c '\''for p in /proc/[0-9]*; do
  case $(tr "\0" " " <"$p/cmdline" 2>/dev/null) in "fern exec "*) printf "%s " "${p#/proc/}" ;; esac
done'\''; echo'
# and samples the node's used memory, in MiB.
# shellcheck disable=SC2016
node_memory='source lab/lib.sh; on_node awk '\''/^MemTotal:/ {t=$2} /^MemAvailable:/ {a=$2} END {print (t-a)/1024}'\'' /proc/meminfo </dev/null'
# Timings are read after the step's wall clock and metrics stop. Only the
# timing lines are collected: provider payloads and service output stay out.
# shellcheck disable=SC2016
trace='source lab/lib.sh
k -n fern-system logs deploy/t3 -c t3 | sed -n "s/^fern-timing //p"
id=t3-$(printf %s "$1" | shasum -a 256 | cut -c1-16)
box_log "$id" | sed -n "/msg=\"claude started\"/p; /msg=\"claude first stdout\"/p" || true'
# Sample host cgroups and the box guest process tree, using metadata only.
# Empty path (before launch) means no box exists yet. Missing boxes during
# restore are expected, but failure to read fixed platform cgroups is fatal.
# shellcheck disable=SC2016
resources='source lab/lib.sh; source lab/hardware.sh
id=t3-$(printf %s "$1" | shasum -a 256 | cut -c1-16)
uid=${2:-}
if [ -z "$uid" ] && [ -n "$1" ]; then uid=$(box_uid "$id"); fi
if [ -n "$uid" ]; then
  echo "BOXUID $uid"
  hardware_sample --counters "$uid"
  if [ "${3:-guest}" = guest ] && fern ls | awk -v id="$id" '\''$1==id && $2=="running" {ok=1} END {exit !ok}'\''; then
    fern exec -i "$id" -- sh -s <lab/hardware-guest.sh || true
  fi
else
  hardware_sample --counters
fi'
# A matched service-only box puts the host pod peak in context. The
# increment includes Claude, tools and runtime growth, not just CLI heap.
# shellcheck disable=SC2016
box_baseline='source lab/lib.sh
id=ck-t3-profile
fern rm "$id" 2>/dev/null || true
fern create "$id" --repo "lab/$1-repo" >/dev/null
fern wake "$id" >/dev/null
uid=$(box_uid "$id")
source lab/hardware.sh
HW_DAEMON=$HW_DAEMON HW_T3=$HW_T3 HW_CONTROLLER=$HW_CONTROLLER hardware_sample --counters "$uid" | awk '\''$1=="box_memory_mib" {print $2}'\''
fern rm "$id" >/dev/null'
T3_RESOURCE_CMD=$resources T3_BOX_BASELINE_CMD=$box_baseline \
T3_PAIR_URL=$pair_url T3_METRICS_CMD=$metrics T3_RM_ENV_CMD=$rm_env T3_SLEEP_CMD=$sleep_env T3_IDLE_CMD=$idle_env T3_NODE_MEMORY_CMD=$node_memory T3_TRACE_CMD=$trace \
  FERN_HOST=$FERN_HOST SCENARIO=${1:-toy} RUNS=${2:-1} MODEL=${3:-claude-haiku-5-5} PARALLEL=${PARALLEL:-1} \
  node lab/t3-run.mjs
