#!/usr/bin/env bash
# The perf gate: runs check.sh's timing sections one at a time (JOBS=1),
# REPEAT times (3; 5 for a baseline), takes each metric's median and
# compares it with the target's baseline, lab/perf/lab.tsv or, with
# FERN_HOST, lab/perf/vps.tsv.
# A metric is worse when it grew by more than its margin and by more than
# 25 ms or 8 MiB; then the gate fails. The margin is TOL (0.25 in the lab,
# whose Docker VM is noisy; 0.15 on a VPS) or, for a metric that spread
# more across the baseline's runs, 1.5 times that spread. Lower is better
# for every metric.
#   lab/perf.sh                  gate the default sections
#   lab/perf.sh box golden       gate only these (metrics of others: skipped)
#   lab/perf.sh --accept         make this run the baseline; commit the file
#   lab/perf.sh --fast           one run, 5 wakes, the quick timing sections:
#                                about 1.5 minutes, with margins twice as
#                                wide; it catches only big regressions
# E2E=N also runs lab/t3-run.sh toy N times (Haiku, your subscription) and
# gates what Fern spent in its turns: the wakes and the revert.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
accept=no fast=no
while [ $# -gt 0 ]; do
  case $1 in
    --accept) accept=yes ;;
    --fast) fast=yes ;;
    *) break ;;
  esac
  shift
done
if [ "$accept$fast" = yesyes ]; then echo "perf: a baseline needs more than one fast run" >&2; exit 2; fi
if [ $# -eq 0 ]; then
  if [ "$fast" = yes ]; then set -- box stream paths golden t3; else set -- box disk stream paths golden t3 many; fi
fi
target=${FERN_HOST:+vps}
target=${target:-lab}
baseline=lab/perf/$target.tsv
tol=${TOL:-$([ "$target" = vps ] && echo 0.15 || echo 0.25)}
repeat=${REPEAT:-$([ "$accept" = yes ] && echo 5 || echo 3)} wakes=${WAKES:-20} widen=1
# One run lands anywhere in the spread the baseline's margins allow for a
# median of three, so fast mode doubles them.
if [ "$fast" = yes ]; then repeat=${REPEAT:-1} wakes=${WAKES:-5} widen=2; fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for i in $(seq "$repeat"); do
  echo "== run $i/$repeat: $*"
  if ! JOBS=1 WAKES=$wakes lab/check.sh "$@" >"$tmp/run$i.out" 2>&1; then
    grep -E '^FAIL' "$tmp/run$i.out" >&2 || tail -20 "$tmp/run$i.out" >&2
    echo "perf: a check failed, so the timings don't count" >&2
    exit 1
  fi
  sed -nE 's/^METRIC //p' "$tmp/run$i.out" >>"$tmp/all"
done

if [ -n "${E2E:-}" ]; then
  echo "== $E2E end-to-end runs through T3 (toy, Haiku)"
  OUT=$tmp/e2e.jsonl lab/t3-run.sh toy "$E2E" >"$tmp/e2e.out" 2>&1 || { tail -20 "$tmp/e2e.out" >&2; exit 1; }
  # What the server spent per step of each run: turn 1's wake (the first,
  # golden or not), the revert's restore and the wake after it.
  # shellcheck disable=SC2016 # JavaScript, not shell
  node -e '
    const lines = require("fs").readFileSync(process.argv[1], "utf8").trim().split("\n");
    const wake = (s) => s && Object.entries(s.server).find(([k]) => k.startsWith("wake_seconds"))?.[1].ms;
    for (const r of lines.map(JSON.parse)) {
      if (!r.pass) { console.error("an end-to-end run failed: " + (r.error ?? "see " + process.argv[1])); process.exit(1); }
      const [t1, , revert, t3] = r.steps;
      if (wake(t1) != null) console.log(`e2e.turn1_wake_ms ${wake(t1)}`);
      console.log(`e2e.revert_ms ${revert.wallMs}`);
      if (wake(t3) != null) console.log(`e2e.wake_after_revert_ms ${wake(t3)}`);
    }' "$tmp/e2e.jsonl" >>"$tmp/all"
fi

# Each metric's median across the runs, and its tolerance: TOL, or 1.5
# times how far its runs spread around the median, if that is more; a
# noisy metric needs a wider margin than a steady one to mean anything.
sort -k1,1 -k2,2n "$tmp/all" | awk -v tol="$tol" '
  function out(  m, t) {
    m = v[int((n + 1) / 2)]
    t = m > 0 ? 1.5 * (v[n] - v[1]) / m : 0
    printf "%s %s %.2f\n", key, m, (t > tol ? t : tol)
  }
  $1 != key { if (key != "") out(); key = $1; n = 0 }
  { v[++n] = $2 }
  END { if (key != "") out() }' >"$tmp/now"

if [ "$accept" = yes ]; then
  mkdir -p lab/perf
  {
    echo "# lab/perf.sh baseline for $target: metric, median of $repeat runs, tolerance;" \
      "$(date -u +%Y-%m-%d), commit $(git rev-parse --short HEAD)$(git diff --quiet HEAD -- . ':!lab/perf' || echo ' with changes')"
    {
      # Metrics of sections this run skipped keep their old values.
      if [ -f "$baseline" ]; then grep -v '^#' "$baseline" | awk 'NR == FNR { now[$1] = 1; next } !($1 in now)' "$tmp/now" -; fi
      cat "$tmp/now"
    } | sort -k1,1
  } >"$tmp/new"
  mv "$tmp/new" "$baseline"
  echo "perf: wrote $baseline; commit it"
  exit 0
fi

[ -f "$baseline" ] || { echo "perf: no baseline $baseline yet; make one with lab/perf.sh --accept" >&2; exit 1; }
echo "== against $baseline ($(sed -n '1s/^# //p' "$baseline"))"
printf "  %-34s %10s %10s %8s %6s\n" metric baseline now change margin
awk -v tol="$tol" -v widen="$widen" '
  NR == FNR { if ($1 !~ /^#/) { base[$1] = $2; margin[$1] = widen * ($3 == "" ? tol : $3) }; next }
  {
    key = $1; now = $2
    if (!(key in base)) { printf "  %-34s %10s %10s %8s %6s  new\n", key, "-", now, "", ""; next }
    b = base[key]; t = margin[key]; floor = (key ~ /_mib/) ? 8 : 25
    change = b > 0 ? (now - b) / b : 0
    verdict = "ok"
    if (now > b * (1 + t) && now - b > floor) { verdict = "WORSE"; worse++ }
    else if (now < b * (1 - t) && b - now > floor) verdict = "better"
    printf "  %-34s %10s %10s %+7.0f%% %5.0f%%  %s\n", key, b, now, 100 * change, 100 * t, verdict
  }
  END {
    if (worse) { printf "perf: FAIL, %d metric(s) worse than the baseline by more than their margin\n", worse; exit 1 }
    print "perf: PASS"
  }' "$baseline" "$tmp/now"
