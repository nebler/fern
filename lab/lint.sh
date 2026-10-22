#!/usr/bin/env bash
# Runs golangci-lint (config in .golangci.yml) in its container.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
exec "${GO_RUN[@]}" -v fern-lab-lintcache:/root/.cache/golangci-lint \
  golangci/golangci-lint:v2.15.0 golangci-lint run "$@"
