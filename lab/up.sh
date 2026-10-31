#!/usr/bin/env bash
# Starts the local lab: one privileged Docker container with systemd as PID
# 1, as a VPS runs Fern (images/Dockerfile, lab-node), a loop-mounted btrfs
# pool at /var/lib/fern, gVisor and Fern's units. Everything it makes is
# named fern-lab* or labelled part-of=fern.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock

if [ -n "$FERN_HOST" ]; then
  echo "FERN_HOST is set; lab/vps.sh sets up a VPS" >&2
  exit 1
fi
if lab_up; then
  echo "$LAB already exists; run lab/down.sh first" >&2
  exit 1
fi
lab/build.sh

# The pool is a sparse image file in its own volume.
docker volume create --label "$LABEL" fern-lab-pool >/dev/null
docker run --rm --label "$LABEL" -v fern-lab-pool:/img --entrypoint sh fern:dev \
  -c 'truncate -s 30G /img/pool.img && mkfs.btrfs -q -f /img/pool.img'

# gVisor, for the boxes: a pinned release, checked, kept beside the pool. The
# node installs it at every start (images/lab-node-start).
url=https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE
case $(docker version -f '{{.Server.Arch}}') in arm64) url+=/aarch64 ;; *) url+=/x86_64 ;; esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -sSfL -o "$tmp/gvisor.tar.bz2" "$url/gvisor.tar.bz2"
if [ "$(shasum -a 512 "$tmp/gvisor.tar.bz2" | cut -d' ' -f1)" != "$(curl -sSfL "$url/gvisor.tar.bz2.sha512" | cut -d' ' -f1)" ]; then
  echo "gVisor's download doesn't match its checksum" >&2
  exit 1
fi
mkdir "$tmp/gvisor" "$tmp/gvisor/bin"
tar -xjf "$tmp/gvisor.tar.bz2" -C "$tmp/gvisor/bin"
tar_dir "$tmp/gvisor" | docker run --rm -i --label "$LABEL" -v fern-lab-pool:/img --entrypoint sh fern:dev \
  -c 'mkdir -p /img/gvisor && tar -x -C /img/gvisor'

# systemd needs /run as a tmpfs, and its own cgroup namespace. noatime (in
# lab-node-start): reading a file mustn't write to the pool, which
# snapshots would then have to flush.
docker build -q --label "$LABEL" --target lab-node -f images/Dockerfile -t fern-lab-node:dev . >/dev/null
docker run -d --name "$LAB" --hostname "$LAB" --label "$LABEL" --privileged --cgroupns=private \
  --tmpfs /run --tmpfs /run/lock -v fern-lab-pool:/var/lib/fern-img \
  -p 127.0.0.1:30773:30773 -p 127.0.0.1:30780:30780 -p 127.0.0.1:16686:16686 fern-lab-node:dev >/dev/null

echo "waiting for systemd"
for _ in $(seq 60); do
  state=$(on_node systemctl is-system-running </dev/null 2>/dev/null || true)
  case $state in running | degraded) break ;; esac
  sleep 1
done

echo "installing fern"
install_cli
load_images fern-box:dev
install_units
on_node systemctl start fern-daemon </dev/null
wait_daemon
echo "lab is up"
