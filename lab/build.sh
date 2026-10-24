#!/usr/bin/env bash
# Builds the lab's images from images/Dockerfile: fern:dev and fern-box:dev,
# and fern-t3:dev with "lab/build.sh t3" or when the lab runs T3, whose image
# carries its own fern CLI. If the lab is up, it loads them, applies
# deploy/fern.yaml (and deploy/t3.yaml with T3) and restarts the pods, since
# the tags stay the same.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
images=(fern fern-box)
t3=no
if [ "${1:-}" = t3 ] || { lab_up && k -n fern-system get deploy/t3 >/dev/null 2>&1; }; then
  t3=yes
  images+=(fern-t3)
fi
for image in "${images[@]}"; do
  docker build -q --label "$LABEL" --target "$image" -f images/Dockerfile -t "$image:dev" . >/dev/null
  echo "built $image:dev"
done
lab_up || exit 0

import_images "${images[@]/%/:dev}"
k apply -f - <deploy/fern.yaml >/dev/null
if [ -n "$FERN_HOST" ]; then
  # In the cluster, the Kubernetes API's address leads to the node's public
  # one, as do NodePorts; boxes may reach neither (architecture §13).
  k -n fern-boxes patch networkpolicy boxes-to-internet-only --type=json \
    -p "[{\"op\": \"add\", \"path\": \"/spec/egress/1/to/0/ipBlock/except/-\", \"value\": \"$FERN_HOST/32\"}]" >/dev/null
fi
k -n fern-system rollout restart ds/fern >/dev/null
k -n fern-system rollout status ds/fern --timeout=300s >/dev/null
echo "restarted the daemon"
if [ "$t3" = yes ]; then
  k apply -f - <deploy/t3.yaml >/dev/null
  k -n fern-system rollout restart deploy/t3 >/dev/null
  k -n fern-system rollout status deploy/t3 --timeout=180s >/dev/null
  echo "restarted T3's server"
fi
