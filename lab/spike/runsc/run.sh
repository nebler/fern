#!/usr/bin/env bash
# Spike #6: cold start, checkpoint and restore of Fern-like boxes under plain
# runsc on FERN_HOST, outside Kubernetes. Holds the lab lock throughout.
#   FERN_HOST=… lab/spike/runsc/run.sh [phase…]   (default: every phase)
# Results land in lab/spike/runsc/results/; summarize.py turns them into
# RESULTS.md's tables. N trials a variant (NBIG for the 400 MB box).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
source "$here/../../lib.sh"
[ -n "$FERN_HOST" ] || { echo "set FERN_HOST" >&2; exit 2; }
lab_lock
N=${N:-15} NBIG=${NBIG:-10}
H=root@$FERN_HOST S=/var/lib/fern/spike
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags spike -o "$here/.probe" ./lab/spike/runsc/probe
"${SSH[@]}" "$H" "[ -d $S ] || btrfs subvolume create $S; mkdir -p $S/code"
scp -q -o BatchMode=yes "$here"/*.py "$here/node-setup.sh" "$here/node-cleanup.sh" "$H:$S/code/"
scp -q -o BatchMode=yes "$here/.probe" "$H:$S/probe" && rm "$here/.probe"
tar -C lab/sample-repo -cf - . | "${SSH[@]}" "$H" "bash $S/code/node-setup.sh"
phases=("$@")
[ ${#phases[@]} -gt 0 ] || phases=(
  "bench.py cold $N" "bench.py cold-cold-cache $N"
  "bench.py ckpt plain $N" "bench.py ckpt flate $N" "bench.py ckpt direct $N" "bench.py ckpt zero $N" "bench.py ckpt big $NBIG"
  "bench.py restore plain $N" "bench.py restore background $N" "bench.py restore flate $N"
  "bench.py restore direct $N" "bench.py restore zero $N"
  "bench.py restore plain-cold $N" "bench.py restore background-cold $N"
  "bench.py restore big $NBIG" "bench.py restore big-background $NBIG"
  "bench.py restore big-cold $NBIG" "bench.py restore big-background-cold $NBIG"
  "correct.py host-edit >>$S/results/correct.jsonl" "correct.py revert >>$S/results/correct.jsonl"
  "correct.py new-ip >>$S/results/correct.jsonl"
  "mount7.py propagate rslave >>$S/results/mount7.jsonl" "mount7.py propagate rprivate >>$S/results/mount7.jsonl"
  "mount7.py subcontainer $N"
)
for p in "${phases[@]}"; do
  "${SSH[@]}" "$H" "cd $S/code && python3 $p" | cut -c1-200
done
mkdir -p "$here/results"
scp -q -o BatchMode=yes "$H:$S/results/*.jsonl" "$here/results/"
# Every sandbox, netns, cgroup and subvolume the spike made.
"${SSH[@]}" "$H" "bash $S/code/node-cleanup.sh"
