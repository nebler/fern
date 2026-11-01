#!/bin/bash
# Removes everything the spike made on the node (run as root there).
set -uo pipefail
S=/var/lib/fern/spike
R=/run/spike-runsc
for id in $(runsc --root=$R list -q 2>/dev/null); do runsc --root=$R delete -force "$id"; done
for ns in $(ip netns list | awk '/^spike-/{print $1}'); do
  ip link del "spk${ns#spike-}h" 2>/dev/null; ip netns del "$ns"
done
for v in $(btrfs subvolume list -o $S 2>/dev/null | awk '{print $NF}' | sort -r); do
  btrfs subvolume delete "/var/lib/fern/$v" >/dev/null # paths are relative to the pool's top
done
[ -d $S ] && { rm -rf --one-file-system "${S:?}"/* 2>/dev/null; btrfs subvolume delete $S; }
btrfs subvolume sync /var/lib/fern >/dev/null
find /sys/fs/cgroup/spike -depth -type d -exec rmdir {} \; 2>/dev/null
# runsc keeps a namespace file mounted here.
umount $R/null-netns 2>/dev/null
rm -rf $R /tmp/spike
