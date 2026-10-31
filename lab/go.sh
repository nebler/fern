#!/usr/bin/env bash
# Runs the go tool in the golang container: lab/go.sh test -race ./...
set -euo pipefail
source "$(dirname "$0")/lib.sh"
sync_src
tty=()
[ -t 0 ] && [ -z "$FERN_HOST" ] && tty=(-t)
"${GO_RUN[@]}" -i "${tty[@]}" golang:1.27 go "$@"
