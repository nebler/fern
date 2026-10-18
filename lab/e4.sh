#!/usr/bin/env bash
# E4: the box holds no state. Writes outside live/ are gone after sleep and
# wake, data in live/ survives, a force-deleted pod recovers Postgres, and
# only declared services come back. Needs lab/up.sh first.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."
ENV=e4 RUNS=${RUNS:-10}

took() { sed -nE 's/.* took ([0-9.]+)ms$/\1/p'; }
sql() { fern exec $ENV -- psql -tAc "$1"; }
# procs lists the box's process names.
procs() { fern exec $ENV -- sh -c 'cat /proc/[0-9]*/comm 2>/dev/null; true' | sort -u | tr '\n' ' '; }

fern rm $ENV 2>/dev/null || true
fern create $ENV --repo lab/sample-repo >/dev/null

echo "== start to ready, $RUNS times"
: > /tmp/fern-e4-wake
for _ in $(seq "$RUNS"); do
  fern wake $ENV | took >> /tmp/fern-e4-wake
  fern sleep $ENV >/dev/null
done
echo "wake (Suspended -> services ready) ms: $(stats < /tmp/fern-e4-wake)"

echo "== state outside live/ is gone, live/ survives"
sql "insert into users (name, email) values ('Edsger', 'edsger@example.com')" >/dev/null
fern exec $ENV -- sh -c 'echo x > /tmp/outside && echo y > inside && (sleep 1000 >/dev/null 2>&1 &)'
check "root filesystem is read-only" 1 "$(fern exec $ENV -- sh -c 'touch /etc/x 2>/dev/null; echo $?')"
check "stray process runs before sleep" 1 "$(procs | grep -cw sleep || true)"
fern sleep $ENV >/dev/null
fern wake $ENV >/dev/null
check "/tmp write is gone" no "$(fern exec $ENV -- sh -c 'test -e /tmp/outside && echo yes || echo no')"
check "live/ write survives" y "$(fern exec $ENV -- cat inside)"
check "database row survives (4 users)" 4 "$(sql "select count(*) from users")"
check "only declared services return (no stray sleep)" 0 "$(procs | grep -cw sleep || true)"
echo "processes after wake: $(procs)"

echo "== force delete while Postgres writes"
sql "insert into users (name, email) values ('Barbara', 'barbara@example.com')" >/dev/null
k -n fern-boxes delete pod $ENV --force --grace-period=0 >/dev/null 2>&1
check "Postgres recovers after force delete (5 users)" 5 "$(sql "select count(*) from users")"
check "the seed ran once" 1 "$(sql "select count(*) from users where name = 'Ada'")"

fern rm $ENV
if [ "$FAILED" = 0 ]; then echo "e4: PASS"; else echo "e4: FAIL"; exit 1; fi
