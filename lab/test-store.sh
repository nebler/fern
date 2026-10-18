#!/usr/bin/env bash
# Runs the store tests against a real btrfs filesystem, in a privileged
# golang container with a loop-mounted pool. Nothing is installed on the Mac.
set -euo pipefail
cd "$(dirname "$0")/.."
exec docker run --rm --privileged --label app.kubernetes.io/part-of=fern \
  -v "$PWD":/src -w /src \
  -v fern-lab-gomod:/go/pkg/mod -v fern-lab-gocache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false -e DEBIAN_FRONTEND=noninteractive \
  golang:1.27 bash -c '
    set -euo pipefail
    apt-get update -qq && apt-get install -y -qq btrfs-progs >/dev/null
    truncate -s 2G /tmp/pool.img && mkfs.btrfs -q /tmp/pool.img
    mkdir -p /mnt/pool && mount -o loop /tmp/pool.img /mnt/pool
    FERN_TEST_BTRFS=/mnt/pool go test -race -count=1 "$@" ./internal/store/
  ' bash "$@"
