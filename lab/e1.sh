#!/usr/bin/env bash
# E1: snapshot cost at 0 MB, 500 MB and 5 GB of data; a restore equals its
# snapshot (tree hash); deleting leaks no subvolumes. Needs lab/up.sh first.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."
ENV=e1 RUNS=${RUNS:-20}
POOL=/var/lib/fern LIVE=/var/lib/fern/envs/e1/live

took() { sed -nE 's/.* took ([0-9.]+)ms$/\1/p'; }
# treehash hashes every file's path and content under a directory.
treehash() { in_daemon "cd '$1' && find . -type f -print0 | sort -z | xargs -0r sha256sum | sha256sum | cut -c1-16"; }

fern rm $ENV 2>/dev/null || true
fern create $ENV >/dev/null
for size in 0 500 5000; do
  echo "== ${size} MB"
  # Data goes straight into live/ from the daemon container: this measures
  # the store, not the box. 100 MB files of random data, plus small files.
  in_daemon "cd $LIVE && rm -rf data && mkdir -p data/small \
    && for i in \$(seq $((size / 100))); do head -c 100M /dev/urandom > data/big-\$i; done \
    && for i in \$(seq 200); do echo \$i > data/small/\$i; done && sync"
  : > /tmp/fern-e1-snap
  for i in $(seq "$RUNS"); do
    fern snapshot $ENV "s$size-$i" | took >> /tmp/fern-e1-snap
  done
  echo "snapshot ms: $(stats < /tmp/fern-e1-snap)"

  want=$(treehash "$POOL/envs/$ENV/snaps/s$size-1")
  in_daemon "echo changed > $LIVE/data/small/1 && rm -f $LIVE/data/small/2 && echo new > $LIVE/data/new"
  : > /tmp/fern-e1-restore
  for _ in $(seq 5); do
    fern restore $ENV "s$size-1" | took >> /tmp/fern-e1-restore
  done
  echo "restore ms (incl. sleep check): $(stats < /tmp/fern-e1-restore)"
  check "restore equals snapshot (tree hash) at ${size} MB" "$want" "$(treehash "$LIVE")"
  in_daemon "btrfs filesystem du -s --human-readable $LIVE $POOL/envs/$ENV/snaps/s$size-1 | tail -2"
done

fern rm $ENV
check "no subvolumes left for $ENV" 0 "$(in_daemon "btrfs subvolume list $POOL | grep -c envs/$ENV/ || true")"
check "no files left for $ENV" no "$(in_daemon "test -e $POOL/envs/$ENV && echo yes || echo no")"
if [ "$FAILED" = 0 ]; then echo "e1: PASS"; else echo "e1: FAIL"; exit 1; fi
