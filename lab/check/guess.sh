# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section guess of lab/check.sh, which sources this file: it has no use by itself.
# hardware is deliberately alone: idle CPU needs a quiet node, and the
# platform sample must not include a concurrently running timing section.
# guess builds the TanStack Start repository without its services.yaml and
# mise.toml, with Postgres in a docker-compose.yml, as a repository that
# knows nothing of Fern: the services and tools are Fern's guess.
guess() {
  echo "== a repository without Fern's files: services and tools guessed"
  local W=/var/lib/fern/worktrees/guess-check G key
  G=$(golden_id guess-check)
  for e in guess1 guess2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  local compose='services:\n  db:\n    image: postgres:16\n    environment:\n      POSTGRES_USER: app\n      POSTGRES_PASSWORD: secret\n'
  for wt in one two; do
    tar_dir lab/tanstack-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - &&
      rm $W/$wt/services.yaml $W/$wt/mise.toml && printf '$compose' >$W/$wt/docker-compose.yml && chown -R 1000:1000 $W/$wt"
  done
  fern create guess1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 300); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric guess.golden_build_ms "golden build from a guess, ms" $(($(now_ms) - start))
  check "a golden snapshot is built from the guess" 1 "$(grep -c . <<<"$key")"
  if [ -z "$key" ]; then
    box_log "$G" | tail -20 | sed 's/^/  builder: /'
    return
  fi
  check "it holds the guessed tools, node_modules and the database, which git ignores" "yes yes yes yes yes" \
    "$(in_daemon "cd /var/lib/fern/envs/$G/snaps/$key && for p in .fern/tools/guess.toml .fern/tools/installs/pnpm node_modules/.pnpm .fern/pg/fern-ready .fern/pg/.gitignore; do test -e \$p && echo yes || echo no; done" | xargs)"
  fern create guess2 --path $W/two >/dev/null
  start=$(now_ms)
  check "a preview wakes the thread and the guessed dev server serves" yes \
    "$(succeeds grep -q 'Hello world' <<<"$(on_node curl -s -H "Host: guess2.localhost:30780" http://127.0.0.1:30780/ </dev/null)")"
  note "asleep to the page through the preview, from a guess: $(($(now_ms) - start)) ms"
  check "Postgres runs as docker-compose's, with its role and database" app \
    "$(fern exec guess2 -- psql -h 127.0.0.1 -U app app -tAc 'select current_user' 2>&1)"
  check "Bash gets mise's Node and pnpm" "yes yes" \
    "$(fern exec guess2 -- sh -c 'for t in node pnpm; do command -v $t | grep -q .fern/tools && echo yes || echo no; done' | xargs)"
  for e in guess1 guess2 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}
