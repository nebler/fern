#!/usr/bin/env bash
# Bonus: gVisor (runsc) in the lab. Installs a pinned gVisor release into the
# lab node (not the Mac), registers it with k3s's containerd as RuntimeClass
# "gvisor", then runs a box under it through Fern's CSI volume.
# Iteration 1's driver is runc only, so the box is switched by patching its
# Sandbox. Needs lab/up.sh first.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."
GVISOR=20261012 ENV=gv RUNS=${RUNS:-5}
ARCH=$(docker version -f '{{.Server.Arch}}'); [ "$ARCH" = arm64 ] && ARCH=aarch64 || ARCH=x86_64
took() { sed -nE 's/.* took ([0-9.]+)ms$/\1/p'; }

if ! docker exec "$LAB" test -x /usr/local/bin/runsc; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  url=https://storage.googleapis.com/gvisor/releases/release/$GVISOR/$ARCH/gvisor.tar.bz2
  curl -sSfL -o "$tmp/gvisor.tar.bz2" "$url"
  want=$(curl -sSfL "$url.sha512" | cut -d' ' -f1)
  [ "$(shasum -a 512 "$tmp/gvisor.tar.bz2" | cut -d' ' -f1)" = "$want" ] || { echo "gVisor checksum mismatch" >&2; exit 1; }
  docker exec -i "$LAB" sh -c 'mkdir -p /usr/local/bin && tar -xjf - -C /usr/local/bin' < "$tmp/gvisor.tar.bz2"
  docker exec -i "$LAB" sh -c 'd=/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d && mkdir -p $d && cat > $d/runsc.toml' <<'TOML'
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
TOML
  docker restart "$LAB" >/dev/null # k3s embeds containerd; this reloads its config
  until k get nodes 2>/dev/null | grep -q ' Ready'; do sleep 1; done
  k -n fern-system rollout status ds/fern --timeout=300s >/dev/null
fi
k apply -f - >/dev/null <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: gvisor, labels: {app.kubernetes.io/part-of: fern}}
handler: runsc
YAML

fern rm $ENV 2>/dev/null || true
fern create $ENV --repo lab/sample-repo >/dev/null
k -n fern-boxes patch sandbox $ENV --type=merge -p '{"spec":{"podTemplate":{"spec":{"runtimeClassName":"gvisor"}}}}' >/dev/null
check "the box runs under gVisor" "Starting gVisor..." "$(fern exec $ENV -- dmesg | head -1 | cut -c16-)"
check "Postgres on the CSI volume (3 users)" 3 "$(fern exec $ENV -- psql -tAc 'select count(*) from users')"
: > /tmp/fern-gv-wake
for i in $(seq "$RUNS"); do
  fern snapshot $ENV "g$i" >/dev/null
  fern exec $ENV -- psql -qc "drop table users"
  fern restore $ENV "g$i" >/dev/null
  fern wake $ENV | took >> /tmp/fern-gv-wake
  check "restore $i brings the table back" 3 "$(fern exec $ENV -- psql -tAc 'select count(*) from users')"
done
echo "gVisor wake after restore ms: $(stats < /tmp/fern-gv-wake)"
fern rm $ENV
if [ "$FAILED" = 0 ]; then echo "gvisor: PASS"; else echo "gvisor: FAIL"; exit 1; fi
