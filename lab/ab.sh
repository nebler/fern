#!/usr/bin/env bash
# Compares two versions of Fern on the same node, in the same minutes:
#
#   lab/ab.sh [-n ROUNDS] [-s "SECTIONS"] [BASE]
#
# A is commit BASE (default HEAD), B the working tree. Both are built and
# imported; then each round runs check.sh's SECTIONS (default box) once on
# each, A B in odd rounds and B A in even ones, so that a node getting
# slower or faster over the run counts against both. The daemon and box
# images switch between runs; check.sh and the CLI are the working tree's.
# A metric that only one version has is listed apart.
#
# The verdict, per metric, comes from lab/ab-compare.py: the medians, B's
# change, a 95% interval for it (bootstrap) and the Mann-Whitney test's p.
# "worse" or "better" needs p < 0.05 and a change of at least 2%; anything
# else is "same". Results go to lab/ab-results/<time>-<host>/. A round of
# box takes about 2 minutes on the VPS; 6 rounds (the default) about 25.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
rounds=6 sections=box
while getopts n:s: opt; do
  case $opt in
    n) rounds=$OPTARG ;;
    s) sections=$OPTARG ;;
    *) sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//' >&2 && exit 2 ;;
  esac
done
shift $((OPTIND - 1))
base=${1:-HEAD}
lab_lock
lab_up || { echo "ab: the lab isn't up" >&2; exit 1; }
if k -n fern-system get deploy/jaeger >/dev/null 2>&1; then
  echo "ab: tracing is on; turn it off first (lab/profile.sh off)" >&2
  exit 1
fi
out=lab/ab-results/$(date -u +%Y%m%dT%H%M%SZ)-${FERN_HOST:-local}
mkdir -p "$out"
src=$(mktemp -d)
trap 'git worktree remove --force "$src/a" >/dev/null 2>&1 || true; rm -rf "$src"' EXIT

git worktree add -q --detach "$src/a" "$base"
{
  echo "A: $(git rev-parse --short "$base") ($base)"
  echo "B: $(git rev-parse --short HEAD) + the working tree's changes"
  echo "sections: $sections; rounds: $rounds; host: ${FERN_HOST:-local}"
} >"$out/versions"
for v in a b; do
  dir=$([ $v = a ] && echo "$src/a" || echo .)
  for image in fern fern-box; do
    docker build -q --label "$LABEL" --target $image -f "$dir/images/Dockerfile" -t "$image:ab-$v" "$dir" >/dev/null
  done
  echo "ab: built $v"
done
import_images fern:ab-a fern-box:ab-a fern:ab-b fern-box:ab-b
install_cli

# use makes version $1's images the node's fern:dev and fern-box:dev and
# restarts the daemon on them. Boxes take the new image at their next wake.
use() {
  local image
  for image in fern fern-box; do
    on_node ctr -n k8s.io images tag --force "docker.io/library/$image:ab-$1" "docker.io/library/$image:dev" >/dev/null
  done
  k -n fern-system rollout restart ds/fern >/dev/null
  k -n fern-system rollout status ds/fern --timeout=300s >/dev/null
}

for r in $(seq "$rounds"); do
  order="a b"
  [ $((r % 2)) -eq 0 ] && order="b a"
  for v in $order; do
    echo "ab: round $r/$rounds, $v"
    use "$v"
    # shellcheck disable=SC2086 # sections are words
    METRICS=1 JOBS=1 lab/check.sh $sections >"$out/$v-$r.out" 2>&1 || echo "ab: a check failed in $v's round $r (see $out/$v-$r.out)" >&2
    sed -nE 's/^METRIC //p' "$out/$v-$r.out" >"$out/$v-$r.metrics"
  done
done

# Leave the node as lab/build.sh would: the working tree's images, as :dev.
for image in fern fern-box; do docker tag "$image:ab-b" "$image:dev"; done
import_images fern:dev fern-box:dev
use b
for v in a b; do
  on_node ctr -n k8s.io images rm "docker.io/library/fern:ab-$v" "docker.io/library/fern-box:ab-$v" >/dev/null
done
python3 -I lab/ab-compare.py "$out" | tee "$out/summary.md"
