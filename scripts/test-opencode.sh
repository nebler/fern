#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-$RANDOM"
NAME="fern-opencode-smoke-${RUN_ID//[^A-Za-z0-9_.-]/}"
VOLUME="fern-$NAME-v2-data"
RUN_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/fern-opencode-smoke.XXXXXX")
REPO="$RUN_ROOT/repository"
CONFIG="$RUN_ROOT/fern.yaml"
FERN_BIN=${FERN_BIN:-}
IMAGE=${FERN_OPENCODE_IMAGE:-"fern/opencode:smoke-$RUN_ID"}
PASSWORD="opencode-smoke-$RUN_ID"
FERN_PID=""
BUILT_IMAGE=0

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "$FERN_PID" ]] && kill -0 "$FERN_PID" 2>/dev/null; then
    kill -TERM "$FERN_PID" 2>/dev/null || true
    wait "$FERN_PID" 2>/dev/null || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker volume rm "$VOLUME" >/dev/null 2>&1 || true
  if [[ "$BUILT_IMAGE" == 1 ]]; then docker image rm "$IMAGE" >/dev/null 2>&1 || true; fi
  rm -rf "$RUN_ROOT"
  exit "$status"
}
trap cleanup EXIT INT TERM

for command in curl docker go jq python3; do
  command -v "$command" >/dev/null || { echo "required command not found: $command" >&2; exit 1; }
done
docker info >/dev/null
mkdir -p "$REPO"
chmod 0777 "$REPO"

if [[ -z ${FERN_OPENCODE_IMAGE:-} ]]; then
  docker build -t "$IMAGE" "$ROOT/images/opencode"
  BUILT_IMAGE=1
else
  docker image inspect "$IMAGE" >/dev/null
fi
if [[ -z "$FERN_BIN" ]]; then
  FERN_BIN="$RUN_ROOT/fern"
  (cd "$ROOT" && GOTOOLCHAIN=local go build -o "$FERN_BIN" ./cmd/fern)
else
  [[ -x "$FERN_BIN" ]] || { echo "FERN_BIN is not executable: $FERN_BIN" >&2; exit 1; }
fi

PROXY_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
PROXY_URL="http://127.0.0.1:$PROXY_PORT"
cat >"$CONFIG" <<EOF
workspace:
  name: $NAME
  image: $IMAGE
  repo: $REPO
  memory: 512Mi
idle:
  after: 1h
proxy:
  listen: 127.0.0.1:$PROXY_PORT
EOF

export OPENCODE_PASSWORD="$PASSWORD"

start_fern() {
  "$FERN_BIN" up -config "$CONFIG" >"$RUN_ROOT/fern.log" 2>&1 &
  FERN_PID=$!
  for _ in {1..350}; do
    if curl -fsS -u "opencode:$PASSWORD" "$PROXY_URL/api/health" >"$RUN_ROOT/health.json" 2>/dev/null; then
      return
    fi
    kill -0 "$FERN_PID" 2>/dev/null || { cat "$RUN_ROOT/fern.log" >&2; return 1; }
    sleep 0.2
  done
  echo "Fern proxy did not become healthy" >&2
  return 1
}

stop_fern() {
  kill -TERM "$FERN_PID"
  wait "$FERN_PID"
  FERN_PID=""
}

start_fern
initial_container_id=$(docker inspect --format '{{.Id}}' "$NAME")
jq -e '.healthy == true and .version == "0.0.0-next-17444"' "$RUN_ROOT/health.json" >/dev/null
[[ $(curl -sS -o /dev/null -w '%{http_code}' "$PROXY_URL/api/health") == 401 ]]

# Exercise the authenticated browser entry point, not just JSON APIs.
curl -fsS -u "opencode:$PASSWORD" -D "$RUN_ROOT/root.headers" -o "$RUN_ROOT/root.html" "$PROXY_URL/"
grep -qi '^content-type: *text/html' "$RUN_ROOT/root.headers"
grep -Eqi '<!doctype html|<html[ >]' "$RUN_ROOT/root.html"
[[ $(curl -sS -o /dev/null -w '%{http_code}' "$PROXY_URL/") == 401 ]]

asset=$(python3 - "$RUN_ROOT/root.html" <<'PY'
import re
import sys

html = open(sys.argv[1], encoding="utf-8").read()
for value in re.findall(r'''(?:src|href)=["']([^"']+)["']''', html, re.I):
    if not value.startswith(("data:", "http:", "https:", "//", "#")):
        print(value)
        break
PY
)
if [[ -n "$asset" ]]; then
  [[ "$asset" == /* ]] || asset="/${asset#./}"
  curl -fsS -u "opencode:$PASSWORD" -o "$RUN_ROOT/ui-asset" "$PROXY_URL$asset"
  [[ -s "$RUN_ROOT/ui-asset" ]]
else
  grep -Eqi '<(script|style)([ >])[^<]+' "$RUN_ROOT/root.html"
fi

curl -fsS -u "opencode:$PASSWORD" -D "$RUN_ROOT/spa.headers" \
  -o "$RUN_ROOT/spa.html" "$PROXY_URL/fern-smoke/direct-navigation"
grep -qi '^content-type: *text/html' "$RUN_ROOT/spa.headers"
grep -Eqi '<!doctype html|<html[ >]' "$RUN_ROOT/spa.html"

set +e
curl -sS -N --max-time 2 -u "opencode:$PASSWORD" "$PROXY_URL/api/event" >"$RUN_ROOT/events.txt"
event_status=$?
set -e
[[ "$event_status" == 28 ]]
grep -q '"type":"server.connected"' "$RUN_ROOT/events.txt"

session=$(curl -fsS -u "opencode:$PASSWORD" -H 'Content-Type: application/json' -d '{}' "$PROXY_URL/api/session")
session_id=$(jq -er '.data.id' <<<"$session")
curl -fsS -u "opencode:$PASSWORD" -H 'Content-Type: application/json' \
  -d '{"command":"sleep 3","timeout":0,"metadata":{}}' "$PROXY_URL/api/shell" >/dev/null
[[ $(curl -fsS -u "opencode:$PASSWORD" "$PROXY_URL/api/shell" | jq '.data | length') == 1 ]]
docker exec "$NAME" test -s /home/user/.local/share/opencode/opencode.db
docker exec "$NAME" sh -c 'mkdir -p "$XDG_CONFIG_HOME/opencode" && printf persisted >"$XDG_CONFIG_HOME/opencode/fern-smoke"'

docker exec -e "OPENCODE_PASSWORD=$PASSWORD" "$NAME" \
  opencode2 api --server http://127.0.0.1:4096 get /api/health | jq -e '.healthy == true' >/dev/null
backend_port=$(docker port "$NAME" 4096/tcp | sed -n '1s/.*://p')
[[ -n "$backend_port" ]]
[[ $(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$backend_port/api/health") == 401 ]]
[[ $(curl -sS -o /dev/null -w '%{http_code}' -u 'opencode:wrong' "http://127.0.0.1:$backend_port/api/health") == 401 ]]

stop_fern
"$FERN_BIN" down -config "$CONFIG"
docker volume inspect "$VOLUME" >/dev/null
start_fern
[[ $(docker inspect --format '{{.Id}}' "$NAME") != "$initial_container_id" ]]
docker exec "$NAME" test -s /home/user/.local/share/opencode/opencode.db
docker exec "$NAME" sh -c 'test "$(cat "$XDG_CONFIG_HOME/opencode/fern-smoke")" = persisted'
curl -fsS -u "opencode:$PASSWORD" "$PROXY_URL/api/session/$session_id" | jq -e ".data.id == \"$session_id\"" >/dev/null
stop_fern

echo "OpenCode smoke test passed: web UI, SPA fallback, auth, health, SSE, shell activity, recreation, session persistence, and config persistence"
