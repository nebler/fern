#!/usr/bin/env bash
# Runs the go tool in the golang container, with the repo mounted and the
# module and build caches kept in volumes. Nothing is installed on the Mac.
set -euo pipefail
cd "$(dirname "$0")/.."
tty=()
[ -t 0 ] && tty=(-t)
exec docker run --rm -i "${tty[@]}" --label app.kubernetes.io/part-of=fern \
  -v "$PWD":/src -w /src \
  -v fern-lab-gomod:/go/pkg/mod -v fern-lab-gocache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  golang:1.27 go "$@"
