#!/usr/bin/env bash
# The iteration-1 slice: the whole disk, database included, is snapshotted
# and restored. Needs lab/up.sh first.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."

count() { fern exec demo -- psql -tAc "select count(*) from users"; }

fern rm demo 2>/dev/null || true
fern create demo --repo lab/sample-repo
check "new environment is asleep" asleep "$(fern ls | awk '$1 == "demo" {print $2}')"
check "first exec wakes the box, 3 users" 3 "$(count)"
fern snapshot demo s1
fern exec demo -- psql -c "drop table users"
check "table is gone" 0 "$(fern exec demo -- psql -tAc "select count(*) from pg_tables where tablename = 'users'")"
fern restore demo s1
check "restore leaves it asleep" asleep "$(fern ls | awk '$1 == "demo" {print $2}')"
check "exec wakes it again, 3 users" 3 "$(count)"
fern rm demo
check "rm removes it" "" "$(fern ls | awk '$1 == "demo"')"

if [ "$FAILED" = 0 ]; then echo "demo-slice: PASS"; else echo "demo-slice: FAIL"; exit 1; fi
