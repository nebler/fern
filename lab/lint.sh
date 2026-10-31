#!/usr/bin/env bash
# Runs the linters in their containers: golangci-lint on the Go (config in
# .golangci.yml; arguments go to it, such as --fix), shellcheck on the
# scripts and hadolint on the Dockerfile (.hadolint.yaml).
set -euo pipefail
source "$(dirname "$0")/lib.sh"
sync_src
"${GO_RUN[@]}" -v fern-lab-lintcache:/root/.cache/golangci-lint \
  golangci/golangci-lint:v2.15.0 golangci-lint run "$@"
builder run --rm --label "$LABEL" "${NET[@]}" -v "$SRC:/mnt" -w /mnt koalaman/shellcheck:v0.11.0 \
  -S warning -x lab/*.sh images/fern-claude images/lab-node-start
builder run --rm -i --label "$LABEL" "${NET[@]}" -v "$SRC/.hadolint.yaml:/.config/hadolint.yaml:ro" \
  hadolint/hadolint:v2.15.1 <images/Dockerfile
