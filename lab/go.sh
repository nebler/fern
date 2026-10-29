#!/usr/bin/env bash
# Runs the go tool in the golang container: lab/go.sh test -race ./...
set -euo pipefail
source "$(dirname "$0")/lib.sh"
tty=()
[ -t 0 ] && tty=(-t)
exec "${GO_RUN[@]}" -i "${tty[@]}" golang:1.27 go "$@"
