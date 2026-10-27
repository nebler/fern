#!/usr/bin/env bash
# Starts the local lab: k3s in one privileged Docker container, with a
# loop-mounted btrfs pool at /var/lib/fern and
# Fern. Everything it makes is named fern-lab* or labelled part-of=fern.
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

# The pool is a sparse image file in its own volume, formatted with the
# daemon image's btrfs-progs (the k3s image has none).
docker volume create --label "$LABEL" fern-lab-pool >/dev/null
docker run --rm --label "$LABEL" -v fern-lab-pool:/img --entrypoint sh fern:dev \
  -c 'truncate -s 30G /img/pool.img && mkfs.btrfs -q -f /img/pool.img'

# gVisor, for the boxes: a pinned release, checked, kept beside the pool. The
# node installs it at every start, before k3s's containerd reads its config.
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
printf '%s\n' "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]" \
  '  runtime_type = "io.containerd.runsc.v1"' >"$tmp/gvisor/runsc.toml"
tar_dir "$tmp/gvisor" | docker run --rm -i --label "$LABEL" -v fern-lab-pool:/img --entrypoint sh fern:dev \
  -c 'mkdir -p /img/gvisor && tar -x -C /img/gvisor'

# "/" must be rshared so the CSI plugin's bind mounts reach the box containers.
# k3s exits when its datastore is slow, as under a big image build; Docker
# then starts it again. Its settings are deploy/k3s-config.yaml, as on a VPS.
# noatime: reading a file mustn't write to the pool, which snapshots would
# then have to flush. gVisor mirrors all of a box's memory mappings into one
# host process; at the kernel's default map count, large builds segfault.
docker run -d --name "$LAB" --hostname "$LAB" --label "$LABEL" --privileged --restart on-failure \
  --tmpfs /run --tmpfs /var/run -v fern-lab-pool:/var/lib/fern-img \
  -p 127.0.0.1:30773:30773 -e K3S_SETTINGS="$(cat deploy/k3s-config.yaml)" \
  --entrypoint /bin/sh "rancher/k3s:${K3S_VERSION/+/-}" -c '
    set -e
    mount --make-rshared /
    mkdir -p /var/lib/fern /etc/rancher/k3s
    mount -o loop,noatime /var/lib/fern-img/pool.img /var/lib/fern
    printf "%s\n" "$K3S_SETTINGS" >/etc/rancher/k3s/config.yaml
    d=/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d
    mkdir -p /usr/local/bin $d
    cp -r /var/lib/fern-img/gvisor/bin/. /usr/local/bin/
    cp /var/lib/fern-img/gvisor/runsc.toml $d/
    sysctl -qw vm.max_map_count=4194304
    exec /bin/k3s server' >/dev/null

echo "waiting for k3s"
for _ in $(seq 120); do
  k get nodes 2>/dev/null | grep -q ' Ready' && break
  sleep 1
done
k get nodes

echo "importing images"
import_images fern:dev fern-box:dev

# Boxes are the daemon's own (runsc); lab/boxes.sh kubernetes installs the
# agent-sandbox controller when pods should run them instead.

echo "installing fern"
k apply -f - <deploy/fern.yaml >/dev/null
k -n fern-system rollout status ds/fern --timeout=300s
echo "lab is up"
