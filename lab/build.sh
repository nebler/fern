#!/usr/bin/env bash
# Builds the lab's images from images/Dockerfile: fern:dev and fern-box:dev,
# and fern-t3:dev with "lab/build.sh t3" or when the node runs T3. If the lab
# is up, it installs the binaries and units, loads the images into the pool
# (fern image load) and restarts the services. Boxes keep running through
# the daemon's restart; it adopts them.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
images=(fern fern-box)
t3=no
if [ "${1:-}" = t3 ] || { lab_up && t3_installed; }; then
  t3=yes
  images+=(fern-t3)
fi
for image in "${images[@]}"; do
  docker build -q --label "$LABEL" --target "$image" -f images/Dockerfile -t "$image:dev" . >/dev/null
  echo "built $image:dev"
done
lab_up || exit 0

install_cli
# fern:dev only carries the binaries install_cli copied.
boxes=("${images[@]:1}")
load_images "${boxes[@]/%/:dev}"
if [ "$t3" = yes ]; then install_units t3; else install_units; fi
restart_daemon
echo "restarted the daemon"
if [ "$t3" = yes ]; then
  on_node systemctl restart fern-t3 </dev/null
  wait_t3
  echo "restarted T3's server"
fi
