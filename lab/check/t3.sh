# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section t3 of lab/check.sh, which sources this file: it has no use by itself.
t3() {
  echo "== T3's server on Fern's node"
  if ! t3_installed || ! wait_t3 2>/dev/null; then
    check "T3's server runs (lab/build.sh t3)" yes no
    return
  fi
  # A VPS's T3 is reached through an SSH tunnel at T3_PORT.
  check "the Mac reaches T3" 200 \
    "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${T3_PORT:-30773}/.well-known/t3/environment")"
  check "T3 runs as uid 1000" 1000 "$(in_t3 'id -u')"
  check "T3 reaches the daemon's socket" yes "$(succeeds in_t3 'fern ls')"
  check "claude outside a workspace is the local one" "$CLAUDE" "$(in_t3 'cd / && claude --version')"
  check "T3 releases idle provider sessions after 5 minutes (fern-sessions.patch)" 1 \
    "$(in_t3 'grep -l T3CODE_PROVIDER_IDLE_TIMEOUT_MS /opt/t3/apps/server/dist/*.mjs | wc -l' || true)"
  # What the checkpoint hook (images/t3/fern-checkpoints.patch) does with a
  # worktree T3 made: adopt it at the first checkpoint, then snapshot,
  # replace and restore.
  local R=/var/lib/fern/repos/t3check W=/var/lib/fern/worktrees/t3check/one
  fern rm t3c 2>/dev/null || true
  in_t3 "rm -rf $R /var/lib/fern/worktrees/t3check && git init -q -b main $R && cd $R && echo v0 > a.txt \
    && git add a.txt && git -c user.email=t@fern -c user.name=t commit -qm init && git worktree add -q -b t3/one $W"
  metric t3.adopt_ms "T3's worktree adopted (first checkpoint), ms" \
    "$(in_t3 "s=\$(date +%s%N); fern create --path $W t3c >/dev/null && echo \$(((\$(date +%s%N) - s) / 1000000))")"
  check "claude in a T3 worktree runs" "$CLAUDE" "$(in_t3 "cd $W && claude --version")"
  # Claude's box: the environment's own, or with agent boxes its agent box.
  local x="--here"
  if [ "${AGENTS:-0}" = 1 ]; then
    x="--here --agent"
    # The agent box sleeps as soon as Claude ends: the sign is the other.
    check "in its agent box: the box with the services stays asleep" asleep "$(state t3c)"
    check "the box with the services has no Claude login or folder" "0 no" \
      "$(in_t3 "cd $W && fern exec --here -- sh -c 'env | grep -c ^CLAUDE_; test -d /var/lib/fern/envs/t3c/claude && echo yes || echo no'" | xargs)"
  else
    check "in its box, which it woke" running "$(state t3c)"
  fi
  # Claude's folder is outside the workspace, so sleep and restore keep it.
  local C=/var/lib/fern/envs/t3c/claude
  check "the box keeps Claude's state outside the workspace" "$C" \
    "$(in_t3 "cd $W && fern exec $x -- sh -c 'echo \$CLAUDE_CONFIG_DIR'")"
  in_t3 "cd $W && fern exec $x -- sh -c 'echo kept > $C/note'"
  # T3 runs as the user that owns Claude's folder, and a line planted there
  # would run in the thread's agent box: its service has the environments
  # read-only (deploy/systemd/fern-t3.service).
  check "T3's service reads an environment's Claude folder, and can't write it" "kept no" \
    "$(in_t3 "cat $C/note" | xargs) $(succeeds in_t3 "touch $C/planted")"
  check "Claude is told it is in a Fern environment" yes \
    "$(in_t3 "cd $W && fern exec $x -- cat $C/CLAUDE.md" | grep -q 'Fern environment' && echo yes || echo no)"
  check "git works in the box" init "$(in_t3 "cd $W && fern exec --here -- git log --format=%s -1")"
  check "and can't change the main repository" no "$(succeeds in_t3 "cd $W && fern exec --here -- touch $R/.git/x")"
  check "or point the worktree's .git elsewhere" no \
    "$(succeeds in_t3 "cd $W && fern exec --here -- sh -c 'echo gitdir: /tmp > .git'")"
  check "claude in a worktree without an environment doesn't run" no \
    "$(succeeds in_t3 "mkdir -p /var/lib/fern/worktrees/t3check/none && cd /var/lib/fern/worktrees/t3check/none && claude --version")"
  # A thread in T3's Local mode works in the main folder, which has no box.
  check "claude in a project's main folder answers T3's probes" "$CLAUDE" "$(in_t3 "cd $R && claude --version")"
  check "but a thread's session there is refused, naming New worktree" yes \
    "$(in_t3 "cd $R && claude --session-id 00000000-0000-4000-8000-000000000000 -p hi 2>&1; true" | grep -q 'New worktree' && echo yes || echo no)"
  # T3 clones a project from GitHub into its home unless told otherwise:
  # projects/, where an update of T3's image keeps it and a worktree's box
  # gets its git folder, but just as boxless.
  check "T3's home is projects/" /var/lib/fern/projects "$(in_t3 "cd \$HOME && pwd")"
  check "and so is one in a project T3 cloned into its home" yes \
    "$(in_t3 "mkdir -p \$HOME/fern-check-clone && cd \$HOME/fern-check-clone && claude --resume 00000000-0000-4000-8000-000000000000 -p hi 2>&1; true" | grep -q 'New worktree' && echo yes || echo no)"
  in_t3 "rm -rf \$HOME/fern-check-clone"
  check "T3's new threads are New worktrees, and deleting one removes it, untracked files and all" "worktree true tracked-changes" \
    "$(in_t3 "node -p 'const s = require(\"/var/lib/fern/userdata/settings.json\"); s.defaultThreadEnvMode + \" \" + s.storageCleanup.worktreeOnDelete + \" \" + s.storageCleanup.worktreeKeepWhen'")"
  check "T3 doesn't offer its browser, or write text with a Codex the image lacks" "false false" \
    "$(in_t3 "node -p 'const s = require(\"/var/lib/fern/userdata/settings.json\"); s.enableAgentBrowserAccess + \" \" + s.providerInstances.codex.enabled'")"
  in_t3 "echo v1 > $W/a.txt && echo junk > $W/ignored.bin"
  metric t3.checkpoint_ms "checkpoint snapshot, ms" "$(resource_op t3.checkpoint snapshot --replace t3c c1 | took)"
  in_t3 "echo v2 > $W/a.txt"
  metric t3.checkpoint_replaced_ms "checkpoint snapshot, replaced, ms" "$(resource_op t3.checkpoint_replaced snapshot --replace t3c c1 | took)"
  in_t3 "echo v3 > $W/a.txt && rm $W/ignored.bin"
  metric t3.restore_ms "restore, ms" "$(resource_op t3.restore restore t3c c1 | took)"
  check "restore brings back the replaced snapshot" v2 "$(in_t3 "cat $W/a.txt")"
  check "and a file git doesn't track" junk "$(in_t3 "cat $W/ignored.bin")"
  check "git works in the restored worktree" "M a.txt" "$(in_t3 "git -C $W status --short --untracked-files=no" | xargs)"
  check "Claude's state outlives the restore" kept "$(in_t3 "cd $W && fern exec $x -- cat $C/note")"
  # The login reaches the box from /etc/fern/claude.env, which a box reads
  # when it starts. Without one, a fake one shows the way works: Claude then
  # fails at Anthropic's API instead of asking for a login.
  if on_node test -s /etc/fern/claude.env </dev/null; then
    fern sleep t3c >/dev/null
    check "Claude's box has its login" yes \
      "$(succeeds in_t3 "cd $W && fern exec $x -- sh -c 'test -n \"\$CLAUDE_CODE_OAUTH_TOKEN\"'")"
    check "T3 has Claude's login" yes "$(succeeds in_t3 "test -n \"\$CLAUDE_CODE_OAUTH_TOKEN\"")"
  else
    echo CLAUDE_CODE_OAUTH_TOKEN=fern-lab-not-a-token | on_node sh -c 'umask 077 && cat >/etc/fern/claude.env'
    fern sleep t3c >/dev/null
    check "Claude in the box uses the login from /etc/fern/claude.env" "API Error: 401" \
      "$(in_t3 "cd $W && claude -p hi 2>&1" 2>/dev/null | grep -o 'API Error: 401' | head -1)"
    on_node rm /etc/fern/claude.env </dev/null
    note "no Claude login stored yet: lab/claude-login.sh"
  fi
  # The box with the services writes the workspace, its tools' folder
  # included, which that box has first on its PATH. Claude's box must not:
  # a file named claude there would run where Claude's login is. Until
  # 2026-11-13 it did.
  if [ "${AGENTS:-0}" = 1 ]; then
    in_t3 "cd $W && printf '[tools]\n' > mise.toml && mkdir -p .fern/tools/shims &&
      printf '%s\n' '#!/bin/sh' 'echo planted' > .fern/tools/shims/claude && chmod +x .fern/tools/shims/claude"
    fern sleep t3c >/dev/null
    check "a claude the workspace plants among its tools doesn't run in Claude's box" "$CLAUDE" "$(in_t3 "cd $W && claude --version")"
    check "no folder of the pool is on the PATH of Claude's box" 0 \
      "$(in_t3 "cd $W && fern exec --here --agent -- sh -c 'echo \$PATH'" | tr ':' '\n' | grep -c '^/var/lib/fern' || true)"
    in_t3 "cd $W && rm -rf mise.toml .fern/tools"
    fern sleep t3c >/dev/null
  fi
  # T3's own git, in a worktree where the box made a repository in a folder
  # and armed it: a filter in that folder's config, and a .gitmodules that
  # asks git to look into it. Staged as a submodule, as T3's own `git add`
  # stages it, git status, diff, add, commit -a, stash and fetch each start
  # a git in that folder, which would run the filter in T3's service
  # (found 2026-11-13). T3's git is fern-git, which opens no repository in
  # a thread's own folder.
  in_t3 "cd $W && fern exec -i --here -- sh -s make" <lab/nested-repo.sh
  in_t3 "cd $W && git add --all 2>/dev/null; git -c user.name=t -c user.email=t@fern commit -qm 'with a folder that has a repository'"
  in_t3 "cd $W && fern exec -i --here -- sh -s arm $W/filter-ran && echo more >> a.txt" <lab/nested-repo.sh
  check "the index holds the box's folder as a submodule, as T3's git add stages it" 1 "$(in_t3 "cd $W && git ls-files --stage | grep -c ^160000" || true)"
  in_t3 "cd $W && { git status --porcelain; git diff HEAD --stat; git add --all; git stash; git stash pop; git fetch --recurse-submodules=yes $R; git -c user.name=t -c user.email=t@fern commit -qam again; } >/dev/null 2>&1" || true
  check "T3's git ran none of what the box's repository names, through seven commands" no "$(succeeds in_t3 "test -e $W/filter-ran")"
  check "and still does its work in the worktree" "again" "$(in_t3 "cd $W && git log -1 --format=%s")"
  check "git in the box's folder itself is refused, with the reason" "128 fern-git: not opening the repository" \
    "$(in_t3 "cd $W/nested && git status >/dev/null 2>&1; echo \$?") $(in_t3 "cd $W/nested && git status 2>&1" | cut -c1-36)"
  in_t3 "cd $W && rm -rf nested .gitmodules filter-ran && git add --all && git -c user.name=t -c user.email=t@fern commit -qm tidy" >/dev/null 2>&1 || true

  # A terminal in T3 (fern-terminals.patch, images/fern-shell): a shell in
  # the thread's box, never one in T3's service, which has every worktree
  # and the server's logins. The pane's own calls, from inside the node: a
  # VPS's T3 has no port the Mac reaches without a tunnel. The worktree has
  # no environment yet, as before a thread's first checkpoint.
  local W2=/var/lib/fern/worktrees/t3check/two pair e2
  e2=t3-$(printf %s "$W2" | shasum -a 256 | cut -c1-16)
  fern rm "$e2" 2>/dev/null || true
  in_t3 "cd $R && git worktree add -q -b t3/two $W2"
  terminal() { # the folder, the line to type, what to wait for
    pair=$(in_t3 "node /opt/t3/apps/server/dist/bin.mjs pair --ttl 2m --label 'fern check'" </dev/null |
      sed -nE 's#^Pairing URL: http://[^/]+/#http://127.0.0.1:30773/#p')
    in_t3 "T3_PAIR_URL='$pair' T3_CWD='$1' T3_TYPE='$2' T3_EXPECT='$3' T3_RESIZE='97 31' node --input-type=module -" <lab/t3-terminal.mjs 2>&1
  }
  # In the box there is no daemon's socket, the login isn't in the
  # environment, and the terminal's size is the pane's.
  check "a terminal in a thread's worktree is a shell in its box, at the pane's size" "running | at=$W2 socket=no login=0 idle=1200 size=31 97" \
    "$(terminal "$W2" 'echo at=$PWD socket=$(test -e /run/fern/fern.sock && echo yes || echo no) login=$(env | grep -c ^CLAUDE_CODE_OAUTH) idle=$TMOUT size=$(stty size)' "at=/[^\r\n]* size=[0-9]+ [0-9]+")"
  # An open terminal keeps its box awake, so the shell ends at an idle
  # prompt: after 20 minutes, here after the 2 seconds it is given.
  check "the shell ends itself at an idle prompt, so the box can sleep" "exited | auto-logout" "$(terminal "$W2" 'TMOUT=2' 'auto-logout')"
  check "the worktree became the thread's environment for it" "$W2" "$(fern ls | awk -v id="$e2" '$1 == id { print $NF }')"
  check "a terminal in a project's main folder starts no shell, and says why" "exited | No terminal here: this folder has no Fern box" \
    "$(terminal "$R" '' 'No terminal here: this folder has no Fern box')"
  check "no shell runs in T3's service" 0 \
    "$(on_node sh -c 'for p in $(cat /sys/fs/cgroup/system.slice/fern-t3.service/cgroup.procs); do cat /proc/$p/comm; done' </dev/null | grep -cxE 'sh|bash|zsh|dash' || true)"
  fern rm "$e2" >/dev/null 2>&1 || true
  in_t3 "fern rm t3c && rm -rf $R /var/lib/fern/worktrees/t3check"
}
