#!/bin/bash
# Prepares the spike on the node (run as root there): the btrfs subvolume
# /var/lib/fern/spike, the fern-box image's root filesystem, and a golden
# workspace from lab/sample-repo (piped in as a tar) seeded by one box run.
set -euo pipefail
S=/var/lib/fern/spike
[ -d $S ] || btrfs subvolume create $S
mkdir -p $S/code $S/ws $S/ckpt /tmp/spike/mnt
if [ ! -x $S/rootfs/usr/local/bin/fern-init ]; then
  ctr -n k8s.io images mount docker.io/library/fern-box:dev /tmp/spike/mnt >/dev/null
  mkdir -p $S/rootfs && cp -a /tmp/spike/mnt/. $S/rootfs/
  ctr -n k8s.io images unmount /tmp/spike/mnt >/dev/null
  ctr -n k8s.io leases rm /tmp/spike/mnt >/dev/null 2>&1 || true
fi
if [ ! -d $S/golden ]; then
  btrfs subvolume create $S/golden >/dev/null
  tar -C $S/golden -xf - 2>/dev/null
  find $S/golden -name '._*' -delete
  chown -R 1000:1000 $S/golden
  cd $S/code && python3 -c '
import box
box.cg_setup(); box.net_up(1)
box.create_start("seed", box.bundle("seed", 1, box.S + "/golden"))
box.wait_ready(1, 120); box.stop("seed", 1); box.net_down(1)'
fi
