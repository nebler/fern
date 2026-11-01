#!/usr/bin/env bash
# Sets up a VPS as a Fern node for the lab, as root over SSH from the Mac:
# the node as install.sh sets it up (the btrfs pool, gVisor, a firewall),
# then Fern and T3 as systemd services (deploy/systemd) from images built
# from this working tree, not a release. Run it again to update; every step
# skips what is done. Then the lab's scripts work on it with FERN_HOST set:
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

on_node bash -s <<'NODE'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
command -v buildah >/dev/null || { apt-get update -qq && apt-get install -yqq --no-install-recommends buildah >/dev/null; }
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
NODE

# The rest of the node, as a released install sets it up: packages, the
# firewall, the pool, the kernel's settings and gVisor.
on_node bash -s -- --node-only <install.sh

# The services' first start comes with the images: build.sh loads them,
# installs the units and starts them.
lab/build.sh t3
echo "$FERN_HOST is up"
