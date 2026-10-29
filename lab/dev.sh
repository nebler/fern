#!/usr/bin/env bash
# The loop after a change, about a minute: unit tests and the image build
# side by side, then the build goes into the lab and check.sh's quick
# sections run against it. Name sections to run those instead:
# lab/dev.sh stream golden. Stops at the first step that fails. Needs
# lab/up.sh (or lab/setup.sh); FERN_HOST works too, but a VPS is for
# lab/perf.sh and the demo, not for every change.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
start=$(date +%s)
step() { echo "== $1 ($(($(date +%s) - start)) s)"; }

step "unit tests and build"
lab/go.sh test -race ./... >"$tmp/test.out" 2>&1 &
tests=$!
if ! lab/build.sh >"$tmp/build.out" 2>&1; then
  cat "$tmp/build.out" >&2
  echo "dev: the build failed" >&2
  exit 1
fi
if ! wait "$tests"; then
  grep -vE '^(ok|\?) ' "$tmp/test.out" >&2
  echo "dev: unit tests failed" >&2
  exit 1
fi
step "checks: ${*:-quick}"
lab/check.sh "$@" | grep -E '^(FAIL|NOTE|check:)'
step "done"
