#!/usr/bin/env bash
# Builds the lab's images from images/Dockerfile: fern:dev and fern-box:dev,
# and fern-t3:dev with "lab/build.sh t3" or when the node runs T3, in the
# Mac's Docker, or with FERN_HOST on the VPS, with Podman. If the lab
# is up, it installs the binaries and units, loads the images into the pool
# (fern image load) and restarts the services. Boxes keep running through
# the daemon's restart; it adopts them.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
images=(fern fern-box)
t3=no
# FERN_T3=skip leaves T3's image and server as they are: the daemon and the
# box image alone, for a node whose T3 holds a turn that must not end, and
# whose memory T3's build would take from that turn's box.
if [ "${FERN_T3:-}" != skip ] && { [ "${1:-}" = t3 ] || { lab_up && t3_installed; }; }; then
  t3=yes
  images+=(fern-t3)
fi
sync_src
for image in "${images[@]}"; do
  builder build -q --label "$LABEL" "${NET[@]}" --target "$image" -f "$SRC/images/Dockerfile" -t "$image:dev" "$SRC" </dev/null >/dev/null
  echo "built $image:dev"
done
lab_up || exit 0

install_cli
# fern:dev only carries the binaries install_cli copied.
boxes=("${images[@]:1}")
load_images "${boxes[@]/%/:dev}"
# Podman keeps every build's layers, on the disk the pool is a file on: 35 GB
# of them filled a VPS's (2026-11-12). The untagged ones a day old go; newer
# ones are the next build's cache.
# On a disk that is nearly full all the same, the cache goes too: the pool
# has its own copy of what was built, and a cold build next time (7 minutes
# for T3) is better than a server at 95% (VPS, 2026-11-13: a deploy left its
# 75 GB disk there, and the one before it had its own images removed by the
# daemon's keeper as the disk ran low).
if [ -n "$FERN_HOST" ]; then
  builder image prune -f --filter until=24h </dev/null >/dev/null
  if [ "$(on_node sh -c 'df --output=pcent /var/lib/fern-img 2>/dev/null | tail -1 | tr -dc 0-9' </dev/null)" -ge 85 ] 2>/dev/null; then
    builder image prune -af </dev/null >/dev/null
    echo "the server's disk is over 85% full: removed Podman's images, of which the pool has its own copies"
  fi
fi
if [ "$t3" = yes ]; then install_units t3; else install_units; fi
restart_daemon
echo "restarted the daemon"
if [ "$t3" = yes ]; then
  # The daemon's restart carries running turns on; T3's ends them. On a VPS,
  # where someone's thread may be working, wait for the turns (FERN_DRAIN
  # seconds, 600), and leave T3 running its image from before if they don't
  # end: a thread worked for eight hours on 2026-11-12 with a day's fixes
  # waiting on it. FERN_DRAIN=0 restarts it whatever runs, as the lab does.
  drain=${FERN_DRAIN:-$([ -n "$FERN_HOST" ] && echo 600 || echo 0)}
  if [ "$drain" = 0 ] || turns_end "$drain"; then
    on_node systemctl restart fern-t3 </dev/null
    wait_t3
    echo "restarted T3's server"
  else
    echo "T3's server was NOT restarted: a thread is still working. It runs its image from before;" \
      "run lab/build.sh again when the thread is idle, or with FERN_DRAIN=0 to end its turn."
  fi
fi
