# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section commit of lab/check.sh, which sources this file: it has no use by itself.
# commit checks Claude's commit tool on a worktree as T3 makes them: the box
# has the repository's git folder read-only, so the daemon commits on the
# node, as T3's user and with the repository's hooks off.
commit() {
  echo "== Claude commits through the daemon"
  if [ "${AGENTS:-0}" != 1 ]; then
    note "skipped: no agent boxes (lab/boxes.sh agents)"
    return
  fi
  local R=/var/lib/fern/repos/ck-commit W=/var/lib/fern/worktrees/ck-commit/one out
  local as="setpriv --reuid 1000 --regid 1000 --clear-groups env HOME=/var/lib/fern/projects"
  fern rm $ENV 2>/dev/null || true
  in_daemon "rm -rf $R /var/lib/fern/worktrees/ck-commit && install -d -o 1000 -g 1000 $R /var/lib/fern/worktrees/ck-commit /var/lib/fern/projects &&
    cd $R && $as git init -q -b main . && $as git config user.name Check && $as git config user.email check@fern &&
    $as sh -c 'echo v0 > a.txt' && $as git add a.txt && $as git commit -qm init && $as git worktree add -q -b t3/one $W &&
    $as sh -c 'printf \"#!/bin/sh\\ntouch $R/hook-ran\\n\" > .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit'"
  fern create $ENV --path $W >/dev/null
  fern exec $ENV -- sh -c 'echo v1 > a.txt && echo new > b.txt'
  # A repository that asks for a bigger box than the node gives: Claude is
  # told, in its hint, at its box's next start.
  fern exec $ENV -- sh -c 'printf "limits:\n  cpus: 64\n" > services.yaml'
  fern sleep $ENV >/dev/null
  check "Claude is told its box has fewer CPUs than the repository asks for" yes \
    "$(fern exec --agent $ENV -- cat /var/lib/fern/envs/$ENV/claude/CLAUDE.md | grep -q '64 CPUs (the box has' && echo yes || echo no)"
  fern exec $ENV -- rm services.yaml
  check "git in the box can't commit: its git folder is read-only" no \
    "$(succeeds fern exec $ENV -- git -c user.name=x -c user.email=x@x commit -qam x)"
  out=$(mcp $ENV commit '{"message": "from the thread\n\nWith a body."}')
  check "the commit tool says what it committed" yes "$(succeeds grep -q '^Committed 2 files to t3/one as ' <<<"$out")"
  check "the commit is on the thread's branch, by the repository's author" "from the thread Check <check@fern>" \
    "$(in_daemon "cd $R && $as git log -1 --format='%s %an <%ae>' t3/one")"
  check "main is untouched" init "$(in_daemon "cd $R && $as git log -1 --format=%s main")"
  check "the repository's hook didn't run" no "$(succeeds in_daemon "test -e $R/hook-ran")"
  check "the git folder is still all T3's user's" 0 "$(in_daemon "find $R/.git ! -user 1000 | wc -l" | tr -d ' ')"
  check "the box sees the commit" "from the thread" "$(fern exec $ENV -- git log -1 --format=%s)"
  check "and a worktree with nothing left to commit" "" "$(fern exec $ENV -- git status --short)"
  check "which the tool says too" "Nothing to commit: the worktree has no changes." "$(mcp $ENV commit '{"message": "again"}')"
  # A repository the box makes in a folder has a config of the box's own.
  # Staged as a submodule, git add and git status on the node would start a
  # git in it, which runs the filter that config names: the box's program,
  # outside the box (found 2026-11-13). The tool commits around it, twice,
  # runs nothing, and leaves no submodule in the index for T3's git to find.
  fern exec $ENV -- sh -c 'git init -q vendor/x && cd vendor/x && echo f > f && git add f &&
    git -c user.name=x -c user.email=x@x commit -qm theirs &&
    git config filter.mark.clean "sh -c \"echo ran >> /var/lib/fern/worktrees/ck-commit/one/filter-ran; cat\"" &&
    echo "* filter=mark" > .git/info/attributes && echo changed > f &&
    cd ../.. && printf "[submodule \"vendor/x\"]\n\tpath = vendor/x\n\turl = ./vendor/x\n\tignore = none\n" > .gitmodules && echo more >> a.txt'
  out=$(mcp $ENV commit '{"message": "around a nested repository"}')
  fern exec $ENV -- sh -c 'echo again >> a.txt; echo changed again > vendor/x/f'
  out="$out $(mcp $ENV commit '{"message": "and once more"}')"
  check "a repository the box made in a folder is left out, and the tool says so" 2 "$(grep -o 'Left out, because each holds a git repository of its own: vendor/x/' <<<"$out" | wc -l | tr -d ' ')"
  check "its filter didn't run on the node, and the index holds no submodule" "no 0" \
    "$(succeeds in_daemon "test -e $W/filter-ran") $(in_daemon "cd $W && $as git ls-files --stage | grep -c ^160000" || true)"
}
