#!/usr/bin/env bash
# Switches who runs the lab's boxes: lab/boxes.sh runsc | kubernetes.
# With runsc the daemon starts them itself (docs/briefs/boxes-without-kubernetes.md):
# it needs the node's PID and network namespaces. Boxes awake in the other
# mode are put to sleep first.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
mode=${1:?usage: lab/boxes.sh runsc|kubernetes}
case $mode in runsc | kubernetes) ;; *) echo "boxes: runsc or kubernetes" >&2 && exit 2 ;; esac
k -n fern-system rollout status ds/fern --timeout=300s >/dev/null
until fern ls >/dev/null 2>&1; do sleep 1; done
fern ls | awk 'NR > 1 && $2 != "asleep" { print $1 }' | while read -r id; do fern sleep "$id" </dev/null >/dev/null; done
args=$(k -n fern-system get ds fern -o jsonpath='{.spec.template.spec.containers[0].args}')
args=$(python3 -c 'import json,sys; a=[x for x in json.loads(sys.argv[1]) if not x.startswith("--boxes=")]; a+=["--boxes=runsc"] if sys.argv[2]=="runsc" else []; print(json.dumps(a))' "$args" "$mode")
host=false dns=ClusterFirst
[ "$mode" = runsc ] && host=true dns=ClusterFirstWithHostNet
k -n fern-system patch ds fern --type=json -p "[
  {\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/args\", \"value\": $args},
  {\"op\": \"add\", \"path\": \"/spec/template/spec/hostPID\", \"value\": $host},
  {\"op\": \"add\", \"path\": \"/spec/template/spec/hostNetwork\", \"value\": $host},
  {\"op\": \"add\", \"path\": \"/spec/template/spec/dnsPolicy\", \"value\": \"$dns\"}]" >/dev/null
k -n fern-system rollout status ds/fern --timeout=300s >/dev/null
until fern ls >/dev/null 2>&1; do sleep 1; done
# Boxes the other mode left awake, which this daemon doesn't see.
fern ls | awk 'NR > 1 && $2 != "asleep" { print $1 }' | while read -r id; do fern sleep "$id" </dev/null >/dev/null; done
echo "boxes: $mode"
