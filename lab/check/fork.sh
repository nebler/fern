# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section fork of lab/check.sh, which sources this file: it has no use by itself.
# fork: three forks of a thread, taken while its Postgres runs, run their
# "tests" at the same time, each against its own Postgres on :5432
# (roadmap step 7), and go with the thread.
fork() {
  echo "== fork"
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Fern', 'fern@example.com')" >/dev/null
  fern fork -n 3 $ENV >"$TMP/forks" 2>"$TMP/took"
  check "three forks" "$ENV-fork-1 $ENV-fork-2 $ENV-fork-3" "$(xargs <"$TMP/forks")"
  metric fork.three_ms "three forks of a running thread, ms" "$(took <"$TMP/took")"
  local start i
  start=$(now_ms)
  for i in 1 2 3; do
    # Fork i adds i users, then counts them, and its Postgres's port.
    fern exec $ENV-fork-$i -- sh -c "psql -qc \"insert into users (name, email) select 'f$i', 'x' from generate_series(1, $i)\" &&
      psql -tAc 'select count(*) from users' && psql -tAc 'show port'" >"$TMP/fork$i" 2>&1 &
  done
  wait
  metric fork.tests_ms "three forks woken and tested at once, ms" "$(($(now_ms) - start))"
  for i in 1 2 3; do
    check "fork $i has its own database (4 + $i users) on :5432" "$((4 + i)) 5432" "$(xargs <"$TMP/fork$i")"
  done
  check "the thread's database is untouched (4 users)" 4 "$(sql $ENV 'select count(*) from users')"
  if [ "$AGENTS" = 1 ]; then
    # Claude's tools, from its agent box, as Claude calls them.
    check "env_fork" "Forks: $ENV-fork-4, $ENV-fork-5" "$(mcp $ENV env_fork '{"count": 2}')"
    check "env_exec runs in a fork (4 users)" 4 "$(mcp $ENV env_exec "{\"env\": \"$ENV-fork-4\", \"command\": \"psql -tAc 'select count(*) from users'\"}")"
    check "env_exec refuses the thread itself" no "$(succeeds mcp $ENV env_exec "{\"env\": \"$ENV\", \"command\": \"true\"}")"
    check "env_delete" "Deleted $ENV-fork-5." "$(mcp $ENV env_delete "{\"env\": \"$ENV-fork-5\"}")"
  fi
  check "more than 8 forks are refused" no "$(succeeds fern fork -n 9 $ENV)"
  fern rm $ENV
  check "deleting the thread deletes its forks" 0 "$(fern ls | grep -c "^$ENV-fork-" || true)"
}
