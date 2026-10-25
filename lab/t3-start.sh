#!/usr/bin/env bash
# Claude's start cost without a model call. Build and measure under one lock:
# bash -c 'source lab/lib.sh; lab_lock; lab/build.sh t3 && lab/t3-start.sh'
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock
id=turn-start-$$
workspace=/var/lib/fern/worktrees/$id/one
repo=/var/lib/fern/repos/$id
cleanup() {
  fern rm "$id" >/dev/null 2>&1 || true
  in_t3 "rm -rf $repo && rmdir /var/lib/fern/worktrees/$id" || true
}
trap cleanup EXIT
# A real worktree gets the same read-only git mount and login environment
# as a T3 thread, without sending a model prompt.
in_t3 "git init -q -b main $repo && git -C $repo -c user.email=t@fern -c user.name=bench commit -qm init --allow-empty \
  && git -C $repo worktree add -q -b bench $workspace && fern create --path $workspace $id" >/dev/null
fern wake "$id" >/dev/null

# One Node process times each command, so kubectl/docker startup isn't part
# of each sample. The native binary is identical on both sides of gVisor.
in_t3 "BENCH_WORKSPACE=$workspace node --input-type=module - /usr/local/libexec/claude local" <lab/t3-start.mjs
fern exec -i "$id" -- node --input-type=module - claude box <lab/t3-start.mjs
