# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section tools of lab/check.sh, which sources this file: it has no use by itself.
# tools builds a golden snapshot of a TanStack Start repository that pins
# Node 22 and pnpm 10 (lab/tanstack-repo), which the box image hasn't got,
# and serves a thread of it through a preview.
tools() {
  echo "== a repository's pinned tools, in its golden snapshot"
  local W=/var/lib/fern/worktrees/tools-check G key
  G=$(golden_id tools-check)
  for e in tools1 tools2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  fresh $ENV
  check "the box image has no npm or pnpm" "no no" \
    "$(fern exec $ENV -- sh -c 'for t in npm pnpm; do PATH=/usr/local/bin:/usr/bin:/bin command -v $t >/dev/null && echo yes || echo no; done' | xargs)"
  for wt in one two; do
    tar_dir lab/tanstack-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - && chown -R 1000:1000 $W/$wt"
  done
  fern create tools1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 300); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric tools.golden_build_ms "golden build with Node, pnpm and the dependencies, ms" $(($(now_ms) - start))
  check "a golden snapshot is built" 1 "$(grep -c . <<<"$key")"
  if [ -z "$key" ]; then
    box_log "$G" | tail -20 | sed 's/^/  builder: /'
    return
  fi
  check "it holds Node 22, pnpm and node_modules, not the checkout" "yes yes yes no" \
    "$(in_daemon "cd /var/lib/fern/envs/$G/snaps/$key && for p in .fern/tools/installs/node/22 .fern/tools/installs/pnpm node_modules/.pnpm package.json; do test -e \$p && echo yes || echo no; done" | xargs)"
  pool_du tools.golden "/var/lib/fern/envs/$G/snaps/$key"
  metric tools.create_ms "create from golden, ms" "$(node_ms "$FERN_CLI" create tools2 --path $W/two)"
  check "git ignores the tools" '*' "$(in_daemon "cat $W/two/.fern/tools/.gitignore")"
  # preview HOST PATH prints what the front door answers for HOST.
  preview() {
    on_node curl -s -H "Host: $1" "http://127.0.0.1:30780$2" </dev/null
  }
  start=$(now_ms)
  check "a preview wakes the thread and TanStack Start serves its page" yes \
    "$(succeeds grep -q 'Hello world' <<<"$(preview "tools2.localhost:30780" /)")"
  note "asleep to TanStack Start's page through the preview, from golden: $(($(now_ms) - start)) ms"
  # A command wakes the box, as Claude's turn does: the daemon renders the
  # default preview ahead, so the person who opens it then finds it done.
  fern sleep tools2 >/dev/null
  fern exec tools2 -- true >/dev/null
  sleep 30
  start=$(now_ms)
  check "after a command's wake, the page is rendered ahead" yes \
    "$(succeeds grep -q 'Hello world' <<<"$(preview "tools2.localhost:30780" /)")"
  local ahead=$(($(now_ms) - start))
  metric tools.page_after_wake_ms "the page through the preview 30 s after a command woke the box, ms" "$ahead"
  check "… and served at once (under 3 s)" yes "$([ "$ahead" -lt 3000 ] && echo yes || echo no)"
  check "Claude's Bash gets the pinned Node and pnpm" "v22 10" \
    "$(fern exec tools2 -- sh -c 'echo $(node --version | cut -d. -f1) $(pnpm --version | cut -d. -f1)')"
  for e in tools1 tools2 "$G" $ENV; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}
