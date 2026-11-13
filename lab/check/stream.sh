# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section stream of lab/check.sh, which sources this file: it has no use by itself.
stream() {
  echo "== the exec stream"
  fresh $ENV
  fern exec $ENV -- true
  python3 -c 'import json; [print(json.dumps({"type": "user", "n": i, "text": "é☃" * 20})) for i in range(10000)]' >"$TMP/lines"
  check "10,000 JSON lines through cat, unchanged" "$(sha256 <"$TMP/lines")" "$(fern exec -i $ENV -- cat <"$TMP/lines" | sha256)"
  head -c 50000000 /dev/urandom >"$TMP/bin"
  start=$(now_ms)
  got=$(fern exec -i $ENV -- cat <"$TMP/bin" | sha256)
  metric stream.roundtrip_50mb_ms "50 MB round trip (Mac -> box -> Mac), ms" $(($(now_ms) - start))
  check "50 MB of random bytes through cat, unchanged" "$(sha256 <"$TMP/bin")" "$got"
  # Counted on the node: the Mac's link to a VPS is too slow to measure Fern by.
  start=$(now_ms)
  n=$(in_daemon "fern exec $ENV -- head -c 200000000 /dev/zero | wc -c" | tr -d ' ')
  metric stream.stdout_200mb_ms "200 MB of stdout, ms" $(($(now_ms) - start))
  check "200 MB of stdout, every byte" 200000000 "$n"
  check "stdout and stderr stay apart" out "$(fern exec $ENV -- sh -c 'echo out; echo err >&2' 2>/dev/null)"
  check "the exit code passes through" 42 "$(fern exec $ENV -- sh -c 'exit 42' >/dev/null 2>&1; echo $?)"
  check "stdin's end reaches the command" 3 "$(printf 'a\nb\nc\n' | fern exec -i $ENV -- wc -l | tr -d ' ')"
  # Endless input: a command that got it would never end.
  check "without -i the command gets no stdin" yes "$(succeeds in_daemon "timeout 20 fern exec $ENV -- cat </dev/zero")"
  rc=$(in_daemon "(sleep 2; printf '\003') | script -qec 'fern exec -it $ENV -- sleep 30' /dev/null >/dev/null 2>&1; echo \$?" || true)
  check "Ctrl-C in a terminal stops the command (130)" 130 "$rc"
  # A full-screen program, as claude is, gets the terminal raw: Ctrl-C is a byte.
  out=$(in_daemon "(sleep 2; printf '\003\003'; sleep 3) |
    timeout 4 script -qec \"fern exec -it $ENV -- sh -c 'stty raw -echo; dd bs=1 count=2 2>/dev/null | od -An -c'\" /dev/null" || true)
  check "and reaches a program that reads the terminal raw" "003 003" "$(tr -d '\r' <<<"$out" | xargs)"
  out=$(in_daemon "fern exec $ENV -- sh -c 'trap \"echo stopping; exit 7\" TERM; echo ready; while :; do sleep 0.1; done' 2>/dev/null &
    sleep 2; kill -TERM \$!; wait \$!; echo code=\$?")
  check "SIGTERM to fern exec reaches the command (exit 7)" "ready stopping code=7" "${out//$'\n'/ }"
  in_daemon "timeout 3 fern exec $ENV -- sleep 301 || true"
  sleep 2
  check "a client that goes away stops its command" 0 \
    "$(fern exec $ENV -- sh -c 'cat /proc/[0-9]*/cmdline 2>/dev/null | tr "\0" " "' | grep -c 'sleep 301' || true)"
}
