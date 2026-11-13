# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section next of lab/check.sh, which sources this file: it has no use by itself.
# github sets up a GitHub App with a new key, points the daemon at a fake
# GitHub on the node that answers as GitHub's API does for an App installed
# on owner/app, and asks for credentials as T3's git and gh do.
# next: a Next.js app with Postgres in docker-compose.yml and no Fern files
# (lab/next-repo), from a lockfile npm installs, to its page reading the
# database through the DATABASE_URL the guess sets (roadmap step 1).
next() {
  echo "== Next.js without Fern's files"
  local W=/var/lib/fern/worktrees/next-check G key page
  G=$(golden_id next-check)
  for e in next1 next2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  for wt in one two; do
    tar_dir lab/next-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - && chown -R 1000:1000 $W/$wt"
  done
  fern create next1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 600); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric next.golden_build_ms "golden build of the Next.js app, ms" $(($(now_ms) - start))
  check "a golden snapshot is built from the guess" 1 "$(grep -c . <<<"$key")"
  if [ -z "$key" ]; then
    box_log "$G" | tail -20 | sed 's/^/  builder: /'
    return
  fi
  check "it holds npm's node_modules, Next and the database" "yes yes" \
    "$(in_daemon "cd /var/lib/fern/envs/$G/snaps/$key && for p in node_modules/next .fern/pg/fern-ready; do test -e \$p && echo yes || echo no; done" | xargs)"
  fern create next2 --path $W/two >/dev/null
  start=$(now_ms)
  page=$(on_node curl -s -m 300 -H "Host: next2.localhost:30780" http://127.0.0.1:30780/ </dev/null | sed 's/<!-- -->//g')
  metric next.first_page_ms "asleep to Next.js's first page through the preview, ms" $(($(now_ms) - start))
  check "the page reads docker-compose's database as its user" yes \
    "$(succeeds grep -q 'Hello from Next.js, database user app' <<<"$page")"
  if ! grep -q 'Hello from Next' <<<"$page"; then
    box_log next2 | tail -20 | sed 's/^/  next2: /'
  fi
  start=$(now_ms)
  on_node curl -s -m 60 -o /dev/null -H "Host: next2.localhost:30780" http://127.0.0.1:30780/ </dev/null
  metric next.page_ms "the page again, ms" $(($(now_ms) - start))
  for e in next1 next2 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}
