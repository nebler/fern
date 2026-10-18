#!/usr/bin/env bash
# Runs golangci-lint (config in .golangci.yml) in its container.
set -euo pipefail
cd "$(dirname "$0")/.."
exec docker run --rm --label app.kubernetes.io/part-of=fern \
  -v "$PWD":/src -w /src \
  -v fern-lab-gomod:/go/pkg/mod -v fern-lab-gocache:/root/.cache/go-build \
  -v fern-lab-lintcache:/root/.cache/golangci-lint \
  -e GOFLAGS=-buildvcs=false \
  golangci/golangci-lint:v2.15.0 golangci-lint run "$@"
