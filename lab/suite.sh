#!/usr/bin/env bash
# Tests the whole workflow from every angle, the same way each time, and
# prints one summary: correctness (every check.sh section), speed and
# memory against the baseline (lab/perf.sh, scale included), and T3 end
# to end, one thread at a time and PARALLEL (5) at once, on Haiku (your
# subscription). Run it before a tag, and on the VPS (FERN_HOST) for
# numbers that count; about 40 minutes there. The full output stays in the
# folder it names; lab/RESULTS.md gets the summary by hand.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
dir=$(mktemp -d "${TMPDIR:-/tmp}/fern-suite.XXXXXX")
target=${FERN_HOST:-lab}
failed=()
start=$(date +%s)
# part NAME COMMAND... runs one part into $dir/NAME.log and keeps going if it
# fails, so one failure doesn't hide the other parts' results.
part() {
  local name=$1 t=$SECONDS
  shift
  echo "== $name"
  if "$@" >"$dir/$name.log" 2>&1; then
    echo "   passed in $((SECONDS - t)) s"
  else
    echo "   FAILED in $((SECONDS - t)) s; see $dir/$name.log"
    failed+=("$name")
  fi
}
echo "fern suite on $target, commit $(git rev-parse --short HEAD); logs in $dir"
part checks lab/check.sh all
part perf lab/perf.sh
part t3-serial env OUT="$dir/t3-serial.jsonl" lab/t3-run.sh toy "${RUNS:-5}"
part t3-parallel env OUT="$dir/t3-parallel.jsonl" PARALLEL="${PARALLEL:-5}" lab/t3-run.sh toy "${PARALLEL:-5}"
part t3-sample env OUT="$dir/t3-sample.jsonl" lab/t3-run.sh sample 2

echo
echo "== summary: $target, $(date -u +%Y-%m-%d), commit $(git rev-parse --short HEAD), $(($(date +%s) - start)) s"
grep -E '^check: ' "$dir/checks.log" | sed 's/^/checks: /' || true
grep -E 'WORSE|better|^perf: ' "$dir/perf.log" || true
grep -E '^NOTE .*(scale|per awake box|woken at once)' "$dir/checks.log" | sed 's/^NOTE /  /' || true
for p in t3-serial t3-parallel t3-sample; do
  echo "$p:"
  grep -E 'runs passed|at a time|wall p50' "$dir/$p.log" | sed 's/^/  /' || true
done
if [ ${#failed[@]} -gt 0 ]; then echo "suite: FAIL (${failed[*]})"; exit 1; fi
echo "suite: PASS"
