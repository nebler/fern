# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section golden of lab/check.sh, which sources this file: it has no use by itself.
golden() {
  echo "== golden snapshots"
  local W=/var/lib/fern/worktrees/golden-check G key
  G=$(golden_id golden-check)
  for e in gold1 gold2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  for wt in one two; do
    tar_dir lab/sample-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - && chown -R 1000:1000 $W/$wt"
  done
  # The first new worktree of a repository has no golden yet: it starts as
  # it is, and its checkout is built into one in the background.
  fern create gold1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 120); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric golden.build_ms "golden build, ms" $(($(now_ms) - start))
  check "a golden snapshot is built from the first worktree" 1 "$(grep -c . <<<"$key")"
  check "it holds the seeded database, not the checkout" "yes no" \
    "$(in_daemon "cd /var/lib/fern/envs/$G/snaps/$key && { test -e .fern/seeded && echo yes || echo no; } && { test -e seed.sql && echo yes || echo no; }" | xargs)"
  check "the first worktree's source copy is gone" 0 "$(fern snapshots gold1 | grep -c golden-source || true)"
  # The next one starts from it: its database is there before its first wake.
  metric golden.create_ms "create from golden, ms" "$(node_ms "$FERN_CLI" create gold2 --path $W/two)"
  check "the next worktree has the database before its first wake" yes \
    "$(in_daemon "test -e $W/two/.fern/seeded && test -e $W/two/seed.sql" && echo yes || echo no)"
  pool_du golden.snapshot "/var/lib/fern/envs/$G/snaps/$key"
  metric golden.first_wake_ms "first wake from golden, ms" "$(resource_op golden.first_wake wake gold2 | took)"
  metric golden.first_wake_cold_ms "first wake without one (initdb and seed), ms" "$(resource_op golden.first_wake_cold wake gold1 | took)"
  check "and Postgres runs on it (3 users)" 3 "$(sql gold2 'select count(*) from users')"
  for e in gold1 gold2 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"

  # T3 sends a project's checkout when it is added, so that even its first
  # worktree starts from a golden.
  W=/var/lib/fern/worktrees/golden-pre
  G=$(golden_id golden-pre)
  for e in gold3 "$G"; do fern rm "$e" 2>/dev/null || true; done
  tar_dir lab/sample-repo | on_node -i "$FERN_CLI" prebuild golden-pre
  start=$(now_ms)
  key=
  for _ in $(seq 120); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric golden.prebuild_ms "golden build from a project's checkout, ms" $(($(now_ms) - start))
  check "a project's checkout is built into a golden" 1 "$(grep -c . <<<"$key")"
  check "its source environment is gone" no "$(succeeds in_daemon "test -e /var/lib/fern/envs/$G-src")"
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/one && mkdir -p $W/one && tar -x -C $W/one -f - && chown -R 1000:1000 $W/one"
  fern create gold3 --path $W/one >/dev/null
  check "the project's first worktree has the database before its first wake" yes \
    "$(in_daemon "test -e $W/one/.fern/seeded" && echo yes || echo no)"
  for e in gold3 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"

  # A repository's setup script runs in the build, before its services'
  # first start, and what it makes is in the golden.
  W=/var/lib/fern/worktrees/golden-setup
  G=$(golden_id golden-setup)
  for e in gold4 gold5 "$G"; do fern rm "$e" 2>/dev/null || true; done
  for wt in one two; do
    tar_dir lab/sample-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt/.fern && tar -x -C $W/$wt -f - &&
      printf '%s\n' '#!/bin/sh' 'set -eu' 'mkdir -p .fern/built' \
        'if test -e .fern/seeded; then echo after; else echo before; fi > .fern/built/services' \
        'echo by-setup > .fern/built/artifact' > $W/$wt/.fern/setup &&
      chmod +x $W/$wt/.fern/setup && chown -R 1000:1000 $W/$wt"
  done
  fern create gold4 --path $W/one >/dev/null
  key=
  for _ in $(seq 120); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  check "a golden is built from a repository with a setup script" 1 "$(grep -c . <<<"$key")"
  fern create gold5 --path $W/two >/dev/null
  check "the next worktree has what the script made, and the seeded database" "by-setup yes" \
    "$(in_daemon "cat $W/two/.fern/built/artifact; test -e $W/two/.fern/seeded && echo yes || echo no" | xargs)"
  check "the script ran before the services' first start" before "$(in_daemon "cat $W/two/.fern/built/services")"
  check "and the worktree's own script is its checkout's" yes "$(succeeds in_daemon "test -x $W/two/.fern/setup")"
  check "Postgres runs on it (3 users)" 3 "$(sql gold5 'select count(*) from users')"
  for e in gold4 gold5 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"

  # A setup script that fails leaves no golden, and it shows where a person
  # looks: fern doctor lists it, a metric counts it, and the thread's Claude
  # is told, to pass it on. Until 2026-11-13 it was a line in the daemon's
  # log. A build that works clears it.
  W=/var/lib/fern/worktrees/golden-fails
  G=$(golden_id golden-fails)
  for e in gold6 gold7 "$G"; do fern rm "$e" 2>/dev/null || true; done
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/one && mkdir -p $W/one/.fern && tar -x -C $W/one -f - &&
    printf '%s\n' '#!/bin/sh' 'echo no such tool: frobnicate >&2' 'exit 3' > $W/one/.fern/setup &&
    chmod +x $W/one/.fern/setup && chown -R 1000:1000 $W/one"
  fern create gold6 --path $W/one >/dev/null
  for _ in $(seq 120); do
    in_daemon "test -e /var/lib/fern/envs/$G/build-failed.json" 2>/dev/null && break
    sleep 1
  done
  check "a setup script that fails is recorded with its builder, and no golden is made" "yes 0" \
    "$(succeeds in_daemon "grep -q frobnicate /var/lib/fern/envs/$G/build-failed.json") $(fern snapshots "$G" 2>/dev/null | grep -c '^golden-' || true)"
  check "fern doctor lists the failed build, with what the script printed" 1 \
    "$(fern doctor 2>/dev/null | grep -c "^FAIL  golden build .*$G.*frobnicate" || true)"
  check "the daemon's metrics count it" yes \
    "$(succeeds in_daemon "curl -sf http://127.0.0.1:9090/metrics | grep -q '^fern_problems [1-9]'")"
  hint="fern exec gold6"
  agent_boxes && hint="fern exec --agent gold6"
  check "the thread's Claude is told, and to tell the person" yes \
    "$($hint -- cat /var/lib/fern/envs/gold6/claude/CLAUDE.md | tr '\n' ' ' | grep -q 'Tell the person.*starting point failed.*frobnicate' && echo yes || echo no)"
  # A box's own log lasts while it runs: the services' box is up after a command.
  fern exec gold6 -- true
  check "fern logs has the daemon's lines about the thread, and its running box's own log" "yes yes" \
    "$(out=$(fern logs gold6 2>&1); { grep -q 'msg="box woke" env=gold6' <<<"$out" && echo yes || echo no; grep -q "== box gold6's own log" <<<"$out" && echo yes || echo no; } | xargs)"
  in_daemon "printf '%s\n' '#!/bin/sh' 'mkdir -p .fern/built && echo mended > .fern/built/artifact' > $W/one/.fern/setup"
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/two && mkdir -p $W/two/.fern && tar -x -C $W/two -f - &&
    cp -p $W/one/.fern/setup $W/two/.fern/setup && chown -R 1000:1000 $W/two"
  fern create gold7 --path $W/two >/dev/null
  key=
  for _ in $(seq 120); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  check "the mended script builds a golden, and the failure is gone from disk and from doctor" "1 no 0" \
    "$(grep -c . <<<"$key") $(succeeds in_daemon "test -e /var/lib/fern/envs/$G/build-failed.json") $(fern doctor 2>/dev/null | grep -c 'golden build' || true)"
  for e in gold6 gold7 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}
