#!/usr/bin/env bash
# Runs every fuzz test in its golang container for FUZZTIME each (default
# 20s): lab/fuzz.sh, or FUZZTIME=5m lab/fuzz.sh for a longer search. The
# seeds also run with every go test. A failure leaves its input under the
# package's testdata/fuzz/, which go test then runs every time: commit it
# with the fix.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
"${GO_RUN[@]}" -e FUZZTIME="${FUZZTIME:-20s}" golang:1.27 bash -c '
  set -euo pipefail
  for pkg in $(go list ./...); do
    for fuzz in $(go test -list "^Fuzz" "$pkg" | grep "^Fuzz" || true); do
      echo "== $fuzz ($pkg)"
      go test -run "^$" -fuzz "^$fuzz\$" -fuzztime "$FUZZTIME" "$pkg"
    done
  done
'
