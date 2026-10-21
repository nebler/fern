#!/bin/sh
# Runs Postgres with its data in the environment (.fern/pg). On the first
# start it creates the cluster and loads seed.sql, once.
set -eu
PGDATA=.fern/pg
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  rm -rf "$PGDATA"
  mkdir -p .fern
  # --no-sync: initdb's own flush took 5s when ten boxes started at once,
  # and Fern's next snapshot flushes the workspace anyway.
  initdb --no-sync -D "$PGDATA" -U postgres --auth=trust >/dev/null
fi
# Only one box runs per environment, so a pid file here is stale: it came
# from a snapshot or a killed box. Its PID may belong to another process now,
# and Postgres would refuse to start.
rm -f "$PGDATA/postmaster.pid"
postgres -D "$PGDATA" -k /tmp -c listen_addresses='' &
pid=$!
trap 'kill -INT $pid' TERM INT
if [ ! -e .fern/seeded ]; then
  until pg_isready -q; do sleep 0.2; done
  psql -q -v ON_ERROR_STOP=1 -1 -f seed.sql
  touch .fern/seeded
fi
wait $pid
