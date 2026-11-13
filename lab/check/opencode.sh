# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section opencode of lab/check.sh, which sources this file: it has no use by itself.
# opencode checks OpenCode's hibernation (fern exec --hibernate in front of
# `opencode serve`, as fern-opencode runs it): without a model, so it costs
# nothing.
opencode() {
  echo "== OpenCode's server stops when idle, and its agent box sleeps"
  if [ "$AGENTS" != 1 ]; then
    note "no agent boxes on this node: skipped"
    return
  fi
  fresh $ENV
  local out
  # shellcheck disable=SC2016 # the node's shell's
  out=$(on_node sh -s -- "$FERN_CLI" $ENV <<'NODE'
fern=$1 env=$2 port=4999 auth=opencode:pw t=$(mktemp -d)
box() { [ -d /sys/fs/cgroup/fern/$env.agent ] && echo awake || echo asleep; }
OPENCODE_PASSWORD=pw $fern exec --agent --hibernate 3s -e OPENCODE_PASSWORD $env -- \
  opencode serve --hostname=127.0.0.1 --port=$port >$t/out 2>$t/err &
fx=$!
for _ in $(seq 150); do grep -q listening $t/out && break; sleep 0.2; done
echo "printed=$(cat $t/out)"
curl -sN -u $auth http://127.0.0.1:$port/api/event >$t/events &
ce=$!
curl -s -o /dev/null -u $auth -X PUT -H 'Content-Type: application/json' \
  -d '{"config":{"type":"remote","url":"http://127.0.0.1:1/mcp","oauth":false}}' http://127.0.0.1:$port/api/experimental/mcp/probe
id=$(curl -s -u $auth -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:$port/api/session |
  python3 -c 'import json, sys; print(json.load(sys.stdin)["data"]["id"])')
echo "first=$(box)"
for _ in $(seq 60); do [ "$(box)" = asleep ] && break; sleep 0.2; done
echo "idle=$(box)"
echo "refused=$(curl -s -o /dev/null -w '%{http_code}' -u opencode:no http://127.0.0.1:$port/api/info) $(box)"
start=$(date +%s%N)
list=$(curl -s -u $auth http://127.0.0.1:$port/api/session)
echo "wake_ms=$((($(date +%s%N) - start) / 1000000))"
echo "kept=$(echo "$list" | grep -q "$id" && echo yes || echo no) $(box)"
echo "mcp=$(curl -s -u $auth http://127.0.0.1:$port/api/mcp | grep -q '"probe"' && echo yes || echo no)"
echo "stream=$(kill -0 $ce 2>/dev/null && echo open || echo closed) $(grep -c server.connected $t/events)"
kill -TERM $fx
wait $fx
kill $ce 2>/dev/null
rm -rf "$t"
NODE
)
  val() { sed -n "s/^$1=//p" <<<"$out"; }
  check "it prints its own URL, where T3 asked" "opencode server listening on http://127.0.0.1:4999" "$(val printed)"
  check "the agent box runs the server" awake "$(val first)"
  check "it sleeps once nothing is under way" asleep "$(val idle)"
  check "a request without the password doesn't wake it" "401 asleep" "$(val refused)"
  check "the next request wakes it, and the session is kept" "yes awake" "$(val kept)"
  check "T3's MCP server is added again" yes "$(val mcp)"
  check "T3's event stream stays open, with one server.connected" "open 1" "$(val stream)"
  note "the first request after a stop: $(val wake_ms) ms"
}
