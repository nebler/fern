#!/usr/bin/env bash
# The loop after a change, about a minute: unit tests and the image build
# side by side, then the build goes into the lab and check.sh's quick
# sections run against it. Name sections to run those instead:
# lab/dev.sh stream golden. Stops at the first step that fails. Needs
# lab/up.sh (or lab/setup.sh); with FERN_HOST it all runs on the VPS, the
# tests before the build, so the Mac only sends the source.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
start=$(date +%s)
step() { echo "== $1 ($(($(date +%s) - start)) s)"; }

step "unit tests and build"
sync_src
lab/go.sh test -race ./... >"$tmp/test.out" 2>&1 &
tests=$!
# The VPS's few cores run them one at a time.
tested=0
[ -z "$FERN_HOST" ] || { wait "$tests" || tested=$?; tests=; }
if ! lab/build.sh >"$tmp/build.out" 2>&1; then
  cat "$tmp/build.out" >&2
  echo "dev: the build failed" >&2
  exit 1
fi
[ -z "$tests" ] || wait "$tests" || tested=$?
if [ "$tested" != 0 ]; then
  grep -vE '^(ok|\?) ' "$tmp/test.out" >&2
  echo "dev: unit tests failed" >&2
  exit 1
fi
step "checks: ${*:-quick}"
lab/check.sh "$@" | grep -E '^(FAIL|NOTE|check:)'
step "done"
