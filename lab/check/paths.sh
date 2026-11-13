# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section paths of lab/check.sh, which sources this file: it has no use by itself.
paths() {
  echo "== workspaces at a path, as T3's worktrees"
  local W=/var/lib/fern/worktrees/fern-check
  for e in at-path adopted adopted-big; do fern rm $e 2>/dev/null || true; done
  fern create at-path --path $W/main >/dev/null
  check "the box works in the workspace's own path" $W/main "$(fern exec at-path -- pwd)"
  tar_dir lab/sample-repo | fern exec -i at-path -- tar -x -f -
  fern sleep at-path >/dev/null
  check "a workspace at a path runs Postgres (3 users)" 3 "$(sql at-path 'select count(*) from users')"
  check "which finds it from a folder inside" at-path "$(fern which $W/main/scripts)"
  # What fern-claude does for T3: the environment and directory from the cwd.
  check "exec --here runs in the current directory" $W/main/scripts \
    "$(in_daemon "cd $W/main/scripts && fern exec --here -- pwd")"
  check "exec -e passes a variable" sdk-ts \
    "$(in_daemon "cd $W/main && CLAUDE_CODE_ENTRYPOINT=sdk-ts fern exec --here -e CLAUDE_CODE_ENTRYPOINT -- sh -c 'echo \$CLAUDE_CODE_ENTRYPOINT'")"
  # A worktree T3 made before Fern saw it is adopted.
  tar_dir lab/sample-repo | in_daemon "mkdir -p $W/adopted && tar -x -C $W/adopted -f - && chown -R 1000:1000 $W/adopted"
  metric paths.adopt_ms "adopting the sample repo, ms" "$(node_ms "$FERN_CLI" create adopted --path $W/adopted)"
  check "the adopted workspace is a subvolume" 256 "$(in_daemon "stat -c %i $W/adopted")"
  check "the adopted workspace runs Postgres (3 users)" 3 "$(sql adopted 'select count(*) from users')"
  in_daemon "mkdir -p $W/big && cd $W/big && seq 20000 | xargs -n 2000 sh -c 'for i; do echo \$i > f\$i; done' _ && chown -R 1000:1000 $W/big"
  metric paths.adopt_20k_ms "adopting 20,000 files, ms" "$(node_ms "$FERN_CLI" create adopted-big --path $W/big)"
  check "all 20,000 files adopted" 20000 "$(in_daemon "ls $W/big | wc -l" | tr -d ' ')"
  # T3 reuses a checkpoint's name after a revert, so the snapshot is taken again.
  fern snapshot adopted c1 >/dev/null
  sql adopted 'delete from users' >/dev/null
  fern snapshot --replace adopted c1 >/dev/null
  sql adopted "insert into users (name, email) values ('Grace', 'grace@example.com')" >/dev/null
  fern restore adopted c1 >/dev/null
  check "a replaced snapshot holds the new state (0 users)" 0 "$(sql adopted 'select count(*) from users')"
  fern rmsnap adopted c1
  check "rmsnap removes it" 0 "$(fern snapshots adopted | grep -c '^c1 ' || true)"
  for e in at-path adopted adopted-big; do fern rm $e; done
  check "rm removes the workspaces at their paths" "" "$(in_daemon "ls $W")"
  # The adopted sample started a golden build for its repo.
  fern rm "$(golden_id fern-check)" 2>/dev/null || true
}
