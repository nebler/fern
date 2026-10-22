#!/usr/bin/env bash
# E0: a Sandbox runs on a btrfs subvolume served by Fern's CSI driver.
# Snapshot while Postgres runs, restore, time Suspended -> Running, and check
# that a force-deleted pod comes back. Needs lab/up.sh first.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."
ENV=e0 RUNS=${RUNS:-10}

took() { sed -nE 's/.* took ([0-9.]+)ms$/\1/p'; }
sql() { fern exec $ENV -- psql -tAc "$1"; }
mounts() { docker exec "$LAB" grep -c "/envs/$ENV/live " /proc/self/mountinfo || true; }

fern rm $ENV 2>/dev/null || true
fern create $ENV --repo lab/sample-repo >/dev/null

echo "== Sandbox + CSI inline volume + btrfs"
t0=$(now_ms); n=$(sql "select count(*) from users"); t1=$(now_ms)
check "Postgres runs on live/ (3 users)" 3 "$n"
echo "first wake + query: $((t1 - t0)) ms"
check "pod is owned by its Sandbox" Sandbox "$(k -n fern-boxes get pod $ENV -o jsonpath='{.metadata.ownerReferences[0].kind}')"
check "CSI plugin saw the pod's identity" 1 \
  "$(k -n fern-system logs ds/fern -c fern | grep -c "msg=publish env=$ENV folder=workspace pod=fern-boxes/$ENV " | awk '{print ($1 > 0)}')"
fern exec $ENV -- sh -c 'echo from-box > from-box'
check "box writes are on the pool" from-box "$(in_daemon "cat /var/lib/fern/envs/$ENV/live/from-box")"
in_daemon "echo from-pool > /var/lib/fern/envs/$ENV/live/from-pool"
check "pool writes are in the box" from-pool "$(fern exec $ENV -- cat from-pool)"

echo "== snapshot while Postgres runs, then restore"
sql "insert into users (name, email) values ('Edsger', 'edsger@example.com')" >/dev/null
fern snapshot $ENV running
sql "insert into users (name, email) values ('Barbara', 'barbara@example.com')" >/dev/null
check "5 users before restore" 5 "$(sql "select count(*) from users")"
fern restore $ENV running
check "restored snapshot taken under Postgres recovers (4 users)" 4 "$(sql "select count(*) from users")"

echo "== Suspended -> Running, $RUNS times"
: > /tmp/fern-e0-sleep; : > /tmp/fern-e0-wake
for _ in $(seq "$RUNS"); do
  fern sleep $ENV | took >> /tmp/fern-e0-sleep
  [ "$(mounts)" = 0 ] || { echo "FAIL live/ still mounted after sleep"; FAILED=1; }
  fern wake $ENV | took >> /tmp/fern-e0-wake
done
check "live/ mounted once while awake" 1 "$(mounts)"
echo "sleep (Running -> pod gone) ms: $(stats < /tmp/fern-e0-sleep)"
echo "wake (Suspended -> Ready) ms:   $(stats < /tmp/fern-e0-wake)"

echo "== force-deleted pod is recreated"
old=$(k -n fern-boxes get pod $ENV -o jsonpath='{.metadata.uid}')
t0=$(now_ms)
k -n fern-boxes delete pod $ENV --force --grace-period=0 >/dev/null 2>&1
until new=$(k -n fern-boxes get pod $ENV -o jsonpath='{.metadata.uid}' 2>/dev/null) && [ -n "$new" ] && [ "$new" != "$old" ]; do sleep 0.2; done
k -n fern-boxes wait pod/$ENV --for=condition=Ready --timeout=120s >/dev/null
t1=$(now_ms)
echo "force delete -> new pod ready: $((t1 - t0)) ms"
check "Postgres recovers in the new pod (4 users)" 4 "$(sql "select count(*) from users")"

fern rm $ENV
check "no subvolumes left for $ENV" 0 "$(in_daemon "btrfs subvolume list /var/lib/fern | grep -c envs/$ENV/ || true")"
if [ "$FAILED" = 0 ]; then echo "e0: PASS"; else echo "e0: FAIL"; exit 1; fi
