# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section upgrade of lab/check.sh, which sources this file: it has no use by itself.
upgrade() {
  echo "== commands run on through a daemon restart"
  fresh $ENV
  local out=$TMP/upgrade.out in=$TMP/upgrade.in code pid
  # A command that prints as it goes, for a while, and exits with a code of its own.
  fern exec $ENV -- sh -c 'echo $$ > /tmp/up.pid; for i in $(seq 1 40); do echo line $i; sleep 0.25; done; exit 7' >"$out" 2>&1 &
  pid=$!
  until [ -s "$out" ]; do sleep 0.1; done
  local before
  before=$(fern exec $ENV -- cat /tmp/up.pid)
  restart_daemon
  check "the command is the same process after the restart" "$before" "$(fern exec $ENV -- cat /tmp/up.pid)"
  check "and still running" yes "$(succeeds fern exec $ENV -- kill -0 "$before")"
  code=0
  wait $pid || code=$?
  check "its client gets its exit code" 7 "$code"
  check "and every line, once, in order" "$(seq 1 40 | sed 's/^/line /')" "$(cat "$out")"

  # stdin too: each side of the restart.
  : >"$out"
  rm -f "$in"
  mkfifo "$in"
  fern exec -i $ENV -- sh -c 'while read -r l; do echo "got $l"; done; echo bye' <"$in" >"$out" 2>&1 &
  pid=$!
  exec 9>"$in"
  echo one >&9
  until grep -q 'got one' "$out"; do sleep 0.1; done
  restart_daemon
  echo two >&9
  exec 9>&-
  wait $pid || true
  check "stdin goes on across the restart" "got one
got two
bye" "$(cat "$out")"

  # A Bash call, as Claude makes it, through the agent box's relay.
  if agent_boxes; then
    fern exec $ENV -- rm -f /tmp/bash.runs
    mcp $ENV Bash '{"command": "echo run >> /tmp/bash.runs; echo before; sleep 8; echo after; exit 4"}' >"$out" 2>&1 &
    pid=$!
    until [ "$(fern exec $ENV -- sh -c 'cat /tmp/bash.runs 2>/dev/null' | wc -l)" -ge 1 ]; do sleep 0.2; done
    restart_daemon
    wait $pid || true
    check "a Bash call gets its command's end across the restart" yes "$(succeeds grep -q after "$out")"
    check "and its exit code" yes "$(succeeds grep -q 'exit code 4' "$out")"
    check "and the command ran once" 1 "$(fern exec $ENV -- sh -c 'wc -l < /tmp/bash.runs' | tr -d ' ')"
    # Claude Code gives a tool's answer 60 s to begin, so the relay begins it
    # at once, as a stream, and sends the tool's answer when it is done.
    local call began
    call='{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "Bash", "arguments": {"command": "sleep 4"}}}'
    began=$(fern exec --agent $ENV -- curl -s -o /dev/null -w '%{time_starttransfer} %{time_total} %{content_type}' -m 60 \
      -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" -d "$call" "http://127.0.0.1:7072/mcp/$ENV")
    check "a tool call's answer begins while its command runs, as a stream" "yes yes text/event-stream" \
      "$(awk '{ print ($1 < 2 ? "yes" : "no"), ($2 >= 4 ? "yes" : "no"), $3 }' <<<"$began")"
    # Claude waits for a build with one call, not by looking again and again.
    local job waited
    job=$(mcp $ENV Bash '{"command": "sleep 3; echo built", "run_in_background": true}' | grep -o 'bash_[a-z0-9]*')
    waited=$(mcp $ENV BashOutput "{\"bash_id\": \"$job\", \"wait_seconds\": 60}")
    check "BashOutput waits for a background command to end, and has its output" "yes yes" \
      "$(grep -q 'exited with code 0' <<<"$waited" && echo yes || echo no) $(grep -q built <<<"$waited" && echo yes || echo no)"
  else
    note "skipped the Bash call: no agent boxes (lab/boxes.sh agents)"
  fi

  # A client that dies, with the daemon up, ends its command at once: no grace.
  on_node sh -c "exec $FERN_CLI exec $ENV -- sh -c 'echo \$\$ > /tmp/dies.pid; exec sleep 300'" </dev/null >/dev/null 2>&1 &
  pid=$!
  until fern exec $ENV -- test -s /tmp/dies.pid; do sleep 0.1; done
  local victim
  victim=$(fern exec $ENV -- cat /tmp/dies.pid)
  on_node pkill -KILL -f "dies.pid" </dev/null || true
  wait $pid 2>/dev/null || true
  sleep 2
  check "a client killed with the daemon up ends its command" no "$(succeeds fern exec $ENV -- kill -0 "$victim")"
}
