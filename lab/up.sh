#!/usr/bin/env bash
# Starts the local lab: k3s in one privileged Docker container, with a
# loop-mounted btrfs pool at /var/lib/fern, the agent-sandbox controller and
# Fern. Everything it makes is named fern-lab* or labelled part-of=fern.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."

if docker inspect "$LAB" >/dev/null 2>&1; then
  echo "$LAB already exists; run lab/down.sh first" >&2
  exit 1
fi
lab/build.sh

# The pool is a sparse image file in its own volume, formatted with the
# daemon image's btrfs-progs (the k3s image has none).
docker volume create --label "$LABEL" fern-lab-pool >/dev/null
docker run --rm --label "$LABEL" -v fern-lab-pool:/img --entrypoint sh fern:dev \
  -c 'truncate -s 30G /img/pool.img && mkfs.btrfs -q -f /img/pool.img'

# "/" must be rshared so the CSI plugin's bind mounts reach the box containers.
docker run -d --name "$LAB" --hostname "$LAB" --label "$LABEL" --privileged \
  --tmpfs /run --tmpfs /var/run -v fern-lab-pool:/var/lib/fern-img \
  -p 127.0.0.1:30773:30773 \
  --entrypoint /bin/sh "$K3S_IMAGE" -c '
    set -e
    mount --make-rshared /
    mkdir -p /var/lib/fern
    mount -o loop /var/lib/fern-img/pool.img /var/lib/fern
    exec /bin/k3s server --disable=traefik,metrics-server --write-kubeconfig-mode=644 \
      --kubelet-arg=pod-max-pids=4096' >/dev/null

echo "waiting for k3s"
for _ in $(seq 120); do
  k get nodes 2>/dev/null | grep -q ' Ready' && break
  sleep 1
done
k get nodes

echo "importing images"
docker save --platform "linux/$(docker version -f '{{.Server.Arch}}')" fern:dev fern-box:dev \
  | docker exec -i "$LAB" ctr -n k8s.io images import - >/dev/null

echo "installing agent-sandbox $SANDBOX_VERSION"
k apply --server-side -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$SANDBOX_VERSION/sandbox.yaml" >/dev/null
k -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=300s

echo "installing fern"
k apply -f - < deploy/fern.yaml >/dev/null
k -n fern-system rollout status ds/fern --timeout=300s
echo "lab is up"
