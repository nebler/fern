#!/usr/bin/env bash
# Spike: freeze an idle box (`runsc pause`) instead of stopping it. Does a
# paused box cost nothing, and does it come back with its processes as they
# were? Holds the lab lock throughout. In the local lab, or on FERN_HOST.
#   lab/spike/pause/run.sh [basic] [exec] [swap] [web] [webswap]   (default: all five)
# It prints what it measures; RESULTS.md has the run that was kept.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
source "$here/../../lib.sh"
lab_lock
N=${N:-10}
ENV=sp
W=/var/lib/fern/worktrees/pause-spike

# node runs one of node.sh's functions on the node.
node() { on_node bash -s -- "$@" <"$here/node.sh"; }
now_ms() { local t=${EPOCHREALTIME//[!0-9]/}; echo $((t / 1000)); }
sql() { fern exec "$1" -- psql -tAc "$2" </dev/null; }
stats() {
  sort -n | awk '{ a[NR] = $1 } END { if (NR == 0) { print "n=0"; exit }
    p = int(0.95 * NR); if (p < 0.95 * NR) p++
    printf "n=%d p50=%s p95=%s max=%s\n", NR, a[int((NR + 1) / 2)], a[p], a[NR] }'
}
say() { printf '\n== %s\n' "$*"; }
# The daemon's idle clocks would stop a box the spike holds paused.
keep_awake() { fern wake "$1" >/dev/null; }

basic() {
  say "the node"
  node swaps
  say "a box with Postgres: pause and resume, $N times"
  fern rm $ENV 2>/dev/null || true
  fern create $ENV --repo lab/sample-repo >/dev/null
  local started
  started=$(sql $ENV "select pg_postmaster_start_time()")
  sql $ENV "insert into users (name, email) values ('Edsger', 'edsger@example.com')" >/dev/null
  # A clock that writes the wall time and the box's uptime five times a second.
  # shellcheck disable=SC2016 # expanded in the box
  fern exec $ENV -- sh -c 'setsid -f sh -c "while :; do echo \$(date +%s.%N) \$(cut -d\" \" -f1 /proc/uptime); sleep 0.2; done >/tmp/ticks 2>&1" && sleep 0.5' </dev/null
  echo "awake: $(node mem $ENV)"
  node cycle $ENV "$N" 1 >"$TMP/cycle"
  echo "pause, ms:                        $(cut -d' ' -f1 "$TMP/cycle" | stats)"
  echo "resume, ms:                       $(cut -d' ' -f2 "$TMP/cycle" | stats)"
  echo "resume returned to fern-init, ms: $(cut -d' ' -f3 "$TMP/cycle" | stats)"
  echo "Postgres is the same process:     $([ "$started" = "$(sql $ENV "select pg_postmaster_start_time()")" ] && echo yes || echo no)"
  echo "its rows:                         $(sql $ENV 'select count(*) from users')"

  say "what a paused box costs, over 10 s"
  keep_awake $ENV
  echo "CPU awake and idle, ms:  $(node cpu $ENV 10)"
  node pause $ENV
  echo "runsc says:              $(node status $ENV)"
  echo "CPU paused, ms:          $(node cpu $ENV 10)"
  echo "memory paused:           $(node mem $ENV)"

  say "the clocks across a 20 s pause"
  sleep 10
  node resume $ENV
  sleep 1
  # The largest gap between two ticks, by the wall clock and by the box's uptime.
  fern exec $ENV -- cat /tmp/ticks </dev/null | awk '
    NR > 1 { w = $1 - pw; u = $2 - pu; if (w > mw) { mw = w; mu = u } } { pw = $1; pu = $2 }
    END { printf "largest gap between two ticks: wall clock %.1f s, the box'"'"'s uptime %.1f s\n", mw, mu }'
}

# exec: what the daemon and a running command make of a pause it didn't ask for.
exec_() {
  say "a command that prints once a second, across a 15 s pause"
  keep_awake $ENV
  # shellcheck disable=SC2016 # expanded in the box
  (fern exec $ENV -- sh -c 'for i in $(seq 25); do echo "$i $(date +%s)"; sleep 1; done' </dev/null >"$TMP/stream" 2>&1 || echo "exit $?" >>"$TMP/stream") &
  local bg=$!
  sleep 4
  node pause $ENV
  sleep 15
  node resume $ENV
  wait $bg || true
  echo "lines that arrived: $(grep -c '^[0-9]' "$TMP/stream") of 25; the last: $(tail -1 "$TMP/stream")"
  awk 'NR > 1 && $2 - p > 2 { printf "a gap of %d s after line %d\n", $2 - p, NR - 1 } { p = $2 }' "$TMP/stream"

  say "a new command to a paused box"
  keep_awake $ENV
  node pause $ENV
  local start
  start=$(now_ms)
  # perl's alarm: macOS has no timeout command.
  (perl -e 'alarm 60; exec @ARGV' "$here/../../fern" exec $ENV -- echo hello </dev/null 2>&1 || echo "exit $?") | tail -2
  echo "after $(($(now_ms) - start)) ms; runsc says: $(node status $ENV)"
  [ "$(node status $ENV)" != paused ] || node resume $ENV
  echo "the daemon's log:"
  on_node journalctl -u fern-daemon --since "-70s" --no-pager -o cat </dev/null | grep -i "$ENV" | tail -6 | cut -c1-220 | sed 's/^/  /'
}

# swap: can a paused box's memory be pushed out, and what does coming back cost?
swap() {
  say "a paused box's memory, pushed to swap"
  [ "$(fern ls | awk -v e=$ENV '$1 == e { print $2 }')" = running ] || fern wake $ENV >/dev/null
  # 300 MB the box really holds, as an agent's process would.
  fern exec $ENV -- sh -c 'setsid -f node -e "const b = Buffer.alloc(300 << 20, 7); setInterval(() => b[0]++, 1000)" >/dev/null 2>&1 && sleep 2' </dev/null
  keep_awake $ENV
  echo "awake, holding 300 MB:  $(node mem $ENV)"
  node pause $ENV
  echo "paused:                 $(node mem $ENV)"
  node reclaim $ENV 1G
  sleep 1
  echo "paused, after reclaim:  $(node mem $ENV)"
  local a
  a=$(now_ms)
  node resume $ENV
  echo "resume to fern-init, ms: $(node ready_ms $ENV) (and $(($(now_ms) - a)) ms seen from here)"
  a=$(now_ms)
  sql $ENV 'select count(*) from users' >/dev/null
  echo "first SQL after it, ms:  $(($(now_ms) - a))"
  sleep 3
  echo "awake again, 3 s later: $(node mem $ENV)"
}

# web: a heavy dev server's first page, from an asleep box and from a paused one.
web() {
  say "an Astro site: the first page from asleep, and from paused"
  local G
  G=$(printf 'golden-pause-spike-%s' "$(printf %s pause-spike | shasum -a 256 | cut -c1-8)")
  for e in spw "$G"; do fern rm "$e" 2>/dev/null || true; done
  tar_dir lab/cloudflare-repo | on_node sh -c "rm -rf $W/one && mkdir -p $W/one && tar -x -C $W/one -f - && chown -R 1000:1000 $W/one"
  fern create spw --path $W/one >/dev/null
  page() { on_node curl -s -o /dev/null -w '%{http_code}' --max-time 180 -H "Host: spw.localhost:30780" "http://127.0.0.1:30780/" </dev/null; }
  local start code
  start=$(now_ms)
  code=$(page)
  echo "the very first page (tools and dependencies installed on the way): $code after $(($(now_ms) - start)) ms"
  for i in 1 2 3; do
    fern sleep spw >/dev/null
    sleep 1
    start=$(now_ms)
    code=$(page)
    echo "asleep to the first page, run $i: $code after $(($(now_ms) - start)) ms"
    sleep 3
  done
  echo "awake with the dev server up: $(node mem spw)"
  for i in 1 2 3; do
    keep_awake spw
    node pause spw
    sleep 5
    start=$(now_ms)
    node resume spw
    code=$(page)
    echo "paused to the first page, run $i: $code after $(($(now_ms) - start)) ms"
    sleep 2
  done
  start=$(now_ms)
  code=$(page)
  echo "awake, for comparison: $code after $(($(now_ms) - start)) ms"
}

# webswap: the same site (web made it), paused with its memory pushed to swap.
webswap() {
  say "the Astro site, paused and pushed to swap"
  page() { on_node curl -s -o /dev/null -w '%{http_code}' --max-time 180 -H "Host: spw.localhost:30780" "http://127.0.0.1:30780/" </dev/null; }
  local start code
  [ "$(page)" = 200 ] || { echo "run the web phase first"; return 1; }
  keep_awake spw
  echo "CPU awake, the dev server idle, over 10 s, ms: $(node cpu spw 10)"
  for i in 1 2 3; do
    keep_awake spw
    node pause spw
    [ "$i" != 1 ] || echo "CPU paused, over 10 s, ms: $(node cpu spw 10)"
    node reclaim spw 2G
    sleep 1
    echo "paused, after reclaim: $(node mem spw)"
    start=$(now_ms)
    node resume spw
    code=$(page)
    echo "paused and swapped out to the first page, run $i: $code after $(($(now_ms) - start)) ms"
    start=$(now_ms)
    code=$(page)
    echo "the page after it: $code after $(($(now_ms) - start)) ms; $(node mem spw)"
  done
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
[ $# -gt 0 ] || set -- basic exec swap web webswap
for phase in "$@"; do
  case $phase in exec) exec_ ;; basic | swap | web | webswap) $phase ;; *) echo "no phase $phase" >&2; exit 2 ;; esac
done
