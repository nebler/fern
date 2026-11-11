#!/usr/bin/env bash
# Runs the store tests against a real btrfs filesystem, in a privileged
# golang container with a loop-mounted pool, and the watch's, which need the
# same privilege for fanotify. Nothing is installed on the Mac.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
sync_src
"${GO_RUN[@]}" --privileged -e DEBIAN_FRONTEND=noninteractive golang:1.27 bash -c '
    set -euo pipefail
    apt-get update -qq && apt-get install -y -qq btrfs-progs >/dev/null
    truncate -s 2G /tmp/pool.img && mkfs.btrfs -q /tmp/pool.img
    mkdir -p /mnt/pool && mount -o loop /tmp/pool.img /mnt/pool
    FERN_TEST_BTRFS=/mnt/pool go test -race -count=1 "$@" ./internal/store/
    # The watch names folders by file handle, which the container'"'"'s own
    # filesystem (overlayfs) lacks: its tests'"'"' folders go on the pool.
    mkdir -p /mnt/pool/tmp
    TMPDIR=/mnt/pool/tmp go test -race -count=1 "$@" ./internal/watch/
  ' bash "$@"
