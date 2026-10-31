#!/usr/bin/env bash
# Sets up a VPS as a Fern node, as root over SSH from the Mac: the btrfs pool,
# gVisor, a firewall, then Fern and T3 as systemd services (deploy/systemd)
# from images built here. Run it again to update; every step skips what is
# done. Then the lab's scripts work on it with FERN_HOST set:
#   FERN_HOST=1.2.3.4 lab/check.sh
# Needs Ubuntu and a firewall in front that lets in only SSH: T3's port must
# not be public. The images are built on the VPS, with Podman.
#
# A VPS set up with k3s before (docs/briefs/fern-without-kubernetes.md)
# moves over: Claude's login from its Secret to /etc/fern/claude.env, boxes
# put to sleep, then k3s removed. The pool stays.
set -euo pipefail
export FERN_HOST=${1:?usage: lab/vps.sh HOST}
source "$(dirname "$0")/lib.sh"
lab_lock

on_node env GVISOR_RELEASE="$GVISOR_RELEASE" bash -s <<'NODE'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
{ command -v mkfs.btrfs && command -v bzip2 && command -v nft && command -v nsenter && command -v podman; } >/dev/null ||
  { apt-get update -qq && apt-get install -yqq btrfs-progs bzip2 nftables iproute2 util-linux curl >/dev/null &&
    apt-get install -yqq --no-install-recommends podman buildah >/dev/null; }
# The Dockerfile's and the lab's short image names are Docker Hub's.
mkdir -p /etc/containers/registries.conf.d
printf 'unqualified-search-registries = ["docker.io"]\nshort-name-mode = "permissive"\n' >/etc/containers/registries.conf.d/fern.conf

# From k3s: Claude's login moves to the file the daemon and T3 read, on the
# node only; boxes sleep, so their services stop cleanly; then k3s goes,
# and the image mounts containerd made in the pool.
if command -v k3s >/dev/null; then
  mkdir -p /etc/fern
  token=$(k3s kubectl -n fern-boxes get secret claude-login -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)
  if [ -n "$token" ] && [ ! -s /etc/fern/claude.env ]; then
    (umask 077 && printf 'CLAUDE_CODE_OAUTH_TOKEN=%s\n' "$token" >/etc/fern/claude.env)
    echo "moved Claude's login to /etc/fern/claude.env"
  fi
  unset token
  if [ -x /usr/local/bin/fern-cli ]; then
    fern-cli ls 2>/dev/null | awk 'NR > 1 && $2 != "asleep" { print $1 }' | while read -r id; do fern-cli sleep "$id" </dev/null >/dev/null; done
  fi
  /usr/local/bin/k3s-uninstall.sh >/dev/null 2>&1
  rm -rf /etc/systemd/system/k3s.service.d /usr/local/bin/fern-cli /var/lib/fern-images
  for m in /var/lib/fern/images/*; do
    if mountpoint -q "$m"; then umount "$m" && rmdir "$m"; fi
  done
  echo "removed k3s"
fi

# The firewall: from the public interface, only SSH may open a connection,
# T3's port included. It loads at boot before Fern. A firewall in front is
# still wise. Its table is its own: the daemon replaces table inet fern,
# the boxes' firewall, whole at every start.
public=$(ip route show default | awk '{ print $5; exit }')
mkdir -p /etc/fern
cat >/etc/fern/firewall.nft <<NFT
table inet fern_host
delete table inet fern_host
table inet fern_host {
  chain in {
    type filter hook prerouting priority -150; policy accept;
    iifname "$public" ct state new tcp dport != 22 drop
    iifname "$public" ct state new udp dport != 68 drop
  }
}
NFT
cat >/etc/systemd/system/fern-firewall.service <<'UNIT'
[Unit]
Description=Fern's firewall: only SSH from outside
# Like Ubuntu's nftables.service: no default dependencies, which would wait
# for sysinit.target, which waits for cloud-init's network stage, which
# waits for the network this unit holds back. The boot hung there.
DefaultDependencies=no
Before=fern-daemon.service fern-t3.service network-pre.target shutdown.target
Wants=network-pre.target
Conflicts=shutdown.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/fern/firewall.nft
[Install]
WantedBy=sysinit.target
UNIT
systemctl daemon-reload
# A node set up before 2026-10-31 has it in multi-user.target.
systemctl disable -q fern-firewall
systemctl enable -q fern-firewall
systemctl restart fern-firewall

# The pool: a sparse image file on the server's own disk, which is faster
# than a network volume. fstab mounts it at boot; Fern's units wait for it.
if ! mountpoint -q /var/lib/fern; then
  mkdir -p /var/lib/fern-img /var/lib/fern
  if [ ! -f /var/lib/fern-img/pool.img ]; then
    truncate -s 50G /var/lib/fern-img/pool.img
    mkfs.btrfs -q /var/lib/fern-img/pool.img
  fi
  grep -q ' /var/lib/fern ' /etc/fstab ||
    echo '/var/lib/fern-img/pool.img /var/lib/fern btrfs loop,noatime,nofail 0 0' >>/etc/fstab
  mount /var/lib/fern
  echo "mounted the pool"
fi

# A pool that doesn't mount at boot must not stop the boot (emergency mode,
# without SSH): Fern's units wait for it instead (RequiresMountsFor).
sed -i -E 's#^(/var/lib/fern-img/pool.img /var/lib/fern btrfs )loop,noatime( |$)#\1loop,noatime,nofail\2#' /etc/fstab

# gVisor mirrors all of a box's memory mappings into one host process; at
# the kernel's default limit, large builds (Node, JVM) segfault. Boxes reach
# the internet through the node, which forwards for them (k3s used to turn
# that on, and a node without it lost it at its next boot).
printf 'vm.max_map_count=4194304\nnet.ipv4.ip_forward=1\n' >/etc/sysctl.d/90-fern.conf
sysctl -q --system

if ! { /usr/local/bin/runsc --version 2>/dev/null | grep -q "release-$GVISOR_RELEASE" && [ -x /usr/local/bin/gvisor-bin/gvisor_sentry ]; }; then
  url=https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE/$(uname -m)
  tmp=$(mktemp -d)
  curl -sSfL -o "$tmp/gvisor.tar.bz2" "$url/gvisor.tar.bz2"
  curl -sSfL -o "$tmp/gvisor.tar.bz2.sha512" "$url/gvisor.tar.bz2.sha512"
  (cd "$tmp" && sha512sum -c --quiet gvisor.tar.bz2.sha512)
  tar -xjf "$tmp/gvisor.tar.bz2" -C /usr/local/bin
  rm -rf "$tmp"
  echo "installed gVisor $GVISOR_RELEASE"
fi
NODE

# The services' first start comes with the images: build.sh loads them,
# installs the units and starts them.
lab/build.sh t3
echo "$FERN_HOST is up"
