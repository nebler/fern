#!/usr/bin/env bash
# Does T3 still take Fern's patches? Builds the stage of images/Dockerfile
# that applies images/t3/*.patch and runs T3's tests of what they touch, on
# T3's newest commit, or the one given: lab/t3-drift.sh [commit]. The image
# pins one commit, so a patch that stopped applying shows only when the pin
# is moved; this says so the day T3 changes
# (.github/workflows/t3-drift.yml runs it every night). T3 takes no feature
# contributions, so the patches stay Fern's to carry. It builds in the Mac's
# Docker and touches neither the lab nor a server.
set -euo pipefail
cd "$(dirname "$0")/.."
pinned=$(sed -n 's/^ARG T3_COMMIT=//p' images/Dockerfile)
commit=${1:-$(git ls-remote https://github.com/pingdotgg/t3code.git HEAD | cut -f1)}
[ -n "$commit" ] || { echo "T3's newest commit can't be read from GitHub" >&2; exit 2; }
echo "T3 at $commit; the image pins $pinned"
if docker build --target t3-build --build-arg "T3_COMMIT=$commit" -f images/Dockerfile . </dev/null; then
  echo "T3 at $commit takes Fern's patches, and its tests of them pass"
else
  echo "T3 at $commit does not take Fern's patches as they are: the build's last lines say which file, or which test" >&2
  exit 1
fi
