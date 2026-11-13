# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section claude of lab/check.sh, which sources this file: it has no use by itself.
claude() {
  echo "== Claude Code in a box"
  fresh $ENV
  check "claude runs" "$CLAUDE" "$(fern exec $ENV -- claude --version)"
  check "the box reaches api.anthropic.com" yes "$(succeeds fern exec $ENV -- curl -s -o /dev/null https://api.anthropic.com/)"
  # Without the login, so it costs nothing: the answer is an error, in stream-json.
  out=$(printf '%s\n' '{"type":"user","message":{"role":"user","content":"say hi"}}' |
    fern exec -i $ENV -- env -u CLAUDE_CODE_OAUTH_TOKEN \
      claude -p --input-format stream-json --output-format stream-json --verbose 2>/dev/null || true)
  check "stream-json arrives as whole JSON lines, the result last" result "$(python3 -c '
import json, sys
try:
    print([json.loads(l) for l in sys.stdin if l.strip()][-1]["type"])
except Exception as e:
    print("broken:", e)' <<<"$out")"
  # Through a terminal, as "fern exec -it" gives it. At this first-run screen
  # claude doesn't quit on two Ctrl-Cs, with or without Fern, so a timeout
  # ends it.
  tui=$(in_daemon "sleep 9 | timeout 8 script -qec 'fern exec -it $ENV -- claude' /dev/null" 2>/dev/null || true)
  # It draws the spaces between words as cursor moves.
  check "claude draws its screen through a terminal" yes "$(succeeds grep -qa Welcome <<<"$tui")"
}
