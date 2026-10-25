#!/usr/bin/env bash
# Sets up a VPS as a Fern node, as root over SSH from the Mac: the btrfs pool,
# k3s with deploy/k3s-config.yaml, gVisor, the agent-sandbox controller, then
# Fern and T3 from images built here. Run it again to update; every step
# skips what is done. Then the lab's scripts work on it with FERN_HOST set:
#   FERN_HOST=1.2.3.4 lab/check.sh
# Needs Ubuntu, the Mac's CPU architecture (the images are built here) and a
# firewall in front that lets in only SSH: T3's NodePort must not be public.
set -euo pipefail
export FERN_HOST=${1:?usage: lab/vps.sh HOST}
source "$(dirname "$0")/lib.sh"
lab_lock

on_node mkdir -p /etc/rancher/k3s </dev/null
on_node tee /etc/rancher/k3s/config.yaml <deploy/k3s-config.yaml >/dev/null
on_node env K3S_VERSION="$K3S_VERSION" GVISOR_RELEASE="$GVISOR_RELEASE" bash -s <<'NODE'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
{ command -v mkfs.btrfs && command -v bzip2; } >/dev/null || { apt-get update -qq && apt-get install -yqq btrfs-progs bzip2 >/dev/null; }

# The firewall: from the public interface, only SSH may open a connection.
# It hooks in before Kubernetes' NAT, which would otherwise pass NodePorts
# (T3) by, and loads at boot before k3s. A firewall in front is still wise.
public=$(ip route show default | awk '{ print $5; exit }')
mkdir -p /etc/fern
cat >/etc/fern/firewall.nft <<NFT
table inet fern
delete table inet fern
table inet fern {
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
Before=k3s.service network-pre.target
Wants=network-pre.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/fern/firewall.nft
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable -q fern-firewall
systemctl restart fern-firewall

# The pool: a sparse image file on the server's own disk, which is faster
# than a network volume. fstab mounts it at boot; k3s waits for it.
if ! mountpoint -q /var/lib/fern; then
  mkdir -p /var/lib/fern-img /var/lib/fern
  if [ ! -f /var/lib/fern-img/pool.img ]; then
    truncate -s 50G /var/lib/fern-img/pool.img
    mkfs.btrfs -q /var/lib/fern-img/pool.img
  fi
  grep -q ' /var/lib/fern ' /etc/fstab ||
    echo '/var/lib/fern-img/pool.img /var/lib/fern btrfs loop,noatime 0 0' >>/etc/fstab
  mount /var/lib/fern
  echo "mounted the pool"
fi
mkdir -p /etc/systemd/system/k3s.service.d
printf '[Unit]\nRequiresMountsFor=/var/lib/fern\n' >/etc/systemd/system/k3s.service.d/fern.conf

# gVisor mirrors all of a box's memory mappings into one host process; at
# the kernel's default limit, large builds (Node, JVM) segfault.
echo vm.max_map_count=4194304 >/etc/sysctl.d/90-fern.conf
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
# Before k3s starts, so its containerd has the runtime from the start.
d=/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d
mkdir -p $d
printf '%s\n' "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]" \
  '  runtime_type = "io.containerd.runsc.v1"' >$d/runsc.toml

if ! k3s --version 2>/dev/null | grep -qF "$K3S_VERSION"; then
  curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="$K3S_VERSION" sh - >/dev/null
  echo "installed k3s $K3S_VERSION"
fi
systemctl daemon-reload
for _ in $(seq 120); do
  kubectl get nodes 2>/dev/null | grep -q ' Ready' && break
  sleep 1
done
kubectl get nodes
NODE

echo "installing agent-sandbox $SANDBOX_VERSION"
k apply --server-side -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$SANDBOX_VERSION/sandbox.yaml" >/dev/null
k -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=300s >/dev/null

# The pods' first start comes with the images: build.sh imports them, applies
# deploy/ and restarts.
lab/build.sh t3
echo "$FERN_HOST is up"
