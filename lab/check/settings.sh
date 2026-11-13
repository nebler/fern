# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section settings of lab/check.sh, which sources this file: it has no use by itself.
# settings: what a repository can plant for Claude. A committed
# .claude/settings.json or .mcp.json would run code in the agent box, next to
# the login. A fake login means no model call and no subscription, and hooks
# and helpers run before any.
settings() {
  echo "== settings in a workspace don't run in the agent box"
  if ! agent_boxes; then
    note "skipped: no agent boxes (lab/boxes.sh agents)"
    return
  fi
  fresh $ENV
  # An agent is a program named claude: this one prints its arguments.
  # shellcheck disable=SC2016 # the box's shell's
  fern exec $ENV -- sh -c 'mkdir -p .fern/bin && printf "#!/bin/sh\nprintf \"%%s\\\\n\" \"\$@\"\n" >.fern/bin/claude && chmod 755 .fern/bin/claude'
  local given
  given=$(fern exec --agent $ENV -- ./.fern/bin/claude -p)
  check "Claude is pointed at the relay in its own box" yes "$(succeeds grep -q '127.0.0.1:7072/mcp/' <<<"$given")"
  check "… and is given no token: the relay adds the box's" no "$(succeeds grep -q 'Authorization' <<<"$given")"
  # Each box has a token of its own, made when it starts. The tool server
  # takes the agent box's and refuses the one of the box that runs the code.
  tool_server() {
    fern exec --agent $ENV -- curl -s -o /dev/null -w '%{http_code}' -m 10 -H "Authorization: Bearer $1" \
      -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
      -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}' \
      "http://fern-daemon:7071/mcp/$ENV"
  }
  # A token lives as long as its box: hold Claude's box awake to read it.
  fern exec --agent $ENV -- sleep 12 </dev/null >/dev/null 2>&1 &
  local hold=$! agent_token box_token
  for _ in $(seq 50); do
    in_daemon "test -s /run/fern/boxes/$ENV.agent/token" && break
    sleep 0.2
  done
  agent_token=$(in_daemon "cat /run/fern/boxes/$ENV.agent/token")
  box_token=$(in_daemon "cat /run/fern/boxes/$ENV/token")
  check "each box has a token of its own" yes "$([ -n "$agent_token" ] && [ -n "$box_token" ] && [ "$agent_token" != "$box_token" ] && echo yes || echo no)"
  check "the tool server takes the agent box's token" 200 "$(tool_server "$agent_token")"
  check "… and refuses the token of the box that runs the code" 401 "$(tool_server "$box_token")"
  check "no token is in the pool" "" "$(in_daemon "grep -rlF -e '${agent_token:-none}' -e '${box_token:-none}' /var/lib/fern/envs/$ENV 2>/dev/null")"
  # Its command over, Claude's box stops. The next start makes a new token,
  # and the old one opens nothing.
  wait $hold 2>/dev/null || true
  check "a token ends with its box" 401 "$(tool_server "$agent_token")"
  # shellcheck disable=SC2016 # the box's shell's
  fern exec $ENV -- sh -c 'mkdir -p .claude
    cat > .claude/m.sh <<"M"
echo ran >> "$PWD/marker-$1"
case "$1" in apiKeyHelper) echo fake-key;; esac
M
    cat > .claude/settings.json <<"J"
{"apiKeyHelper": "sh .claude/m.sh apiKeyHelper", "enableAllProjectMcpServers": true,
 "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "sh .claude/m.sh hook"}]}]}}
J
    cat > .mcp.json <<"J"
{"mcpServers": {"planted": {"type": "stdio", "command": "sh", "args": [".claude/m.sh", "mcpjson"]}}}
J'
  local args='--output-format stream-json --verbose --input-format stream-json --permission-mode bypassPermissions --allow-dangerously-skip-permissions'
  local turn='{"type":"control_request","request_id":"r1","request":{"subtype":"initialize","hooks":null}}
{"type":"user","session_id":"","message":{"role":"user","content":"say ok"},"parent_tool_use_id":null}'
  # The control: started through a shell, Fern adds no flags, and the planted
  # files do run. Without it this check could pass by planting nothing.
  # shellcheck disable=SC2086
  (printf '%s\n' "$turn"; sleep 12) | on_node env CLAUDE_CODE_OAUTH_TOKEN=fake-login $FERN_CLI exec -i -e CLAUDE_CODE_OAUTH_TOKEN --agent $ENV -- sh -c "exec claude $args" >/dev/null 2>&1 || true
  check "control: with no flags the planted files run" yes "$(succeeds fern exec $ENV -- sh -c 'ls marker-* >/dev/null 2>&1')"
  fern exec $ENV -- sh -c 'rm -f marker-*'
  # shellcheck disable=SC2086
  (printf '%s\n' "$turn"; sleep 12) | on_node env CLAUDE_CODE_OAUTH_TOKEN=fake-login $FERN_CLI exec -i -e CLAUDE_CODE_OAUTH_TOKEN --agent $ENV -- claude $args >"$TMP/settings.out" 2>&1 || true
  check "through Fern, none of them run" "" "$(fern exec $ENV -- sh -c 'ls marker-* 2>/dev/null')"
  check "and Claude started" yes "$(succeeds grep -q '"subtype":"init"' "$TMP/settings.out")"
  check "with Fern's MCP server" yes "$(succeeds grep -q '"name":"fern"' "$TMP/settings.out")"
  check "and not the planted one" no "$(succeeds grep -q '"name":"planted"' "$TMP/settings.out")"
}
