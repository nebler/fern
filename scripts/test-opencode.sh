#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-$RANDOM"
NAME="fern-opencode-smoke-${RUN_ID//[^A-Za-z0-9_.-]/}"
VOLUME="fern-$NAME-v2-data"
RUN_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/fern-opencode-smoke.XXXXXX")
REPO="$RUN_ROOT/repository"
CONFIG="$RUN_ROOT/fern.yaml"
ENV_FILE="$RUN_ROOT/fern.env"
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

REMOTE_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
OPERATOR_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
REMOTE_URL="http://127.0.0.1:$REMOTE_PORT"
OPERATOR_URL="http://127.0.0.1:$OPERATOR_PORT"
CONTROL_PASSWORD="control-$PASSWORD"
cat >"$CONFIG" <<EOF
workspace:
  name: $NAME
  image: $IMAGE
  repo: $REPO
  memory: 512Mi
idle:
  after: 1h
proxy:
  listen: 127.0.0.1:$REMOTE_PORT
  operatorListen: 127.0.0.1:$OPERATOR_PORT
  remoteOrigin: https://opencode-smoke.invalid
control:
  password: \${FERN_CONTROL_PASSWORD}
EOF
printf 'OPENCODE_PASSWORD=%s\nFERN_CONTROL_PASSWORD=%s\n' "$PASSWORD" "$CONTROL_PASSWORD" >"$ENV_FILE"
chmod 0600 "$ENV_FILE"

start_fern() {
  "$FERN_BIN" up -config "$CONFIG" -env-file "$ENV_FILE" >"$RUN_ROOT/fern.log" 2>&1 &
  FERN_PID=$!
  for _ in {1..350}; do
    if curl -fsS -u "opencode:$PASSWORD" "$OPERATOR_URL/api/health" >"$RUN_ROOT/health.json" 2>/dev/null; then
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
[[ $(curl -sS -o /dev/null -w '%{http_code}' "$OPERATOR_URL/api/health") == 401 ]]
[[ $(curl -sS -o /dev/null -w '%{http_code}' -u "opencode:$PASSWORD" "$REMOTE_URL/api/health") == 401 ]]
[[ $(curl -sS -o /dev/null -w '%{http_code}' -u "fern:$CONTROL_PASSWORD" "$REMOTE_URL/api/health") == 401 ]]

pair_code=$(curl -fsS -u "fern:$CONTROL_PASSWORD" -X POST "$OPERATOR_URL/fern/pair/new" | jq -er .code)
curl -fsS "$REMOTE_URL/fern/pair?code=$pair_code" | grep -q 'Pair this phone?'
curl -sS -D "$RUN_ROOT/pair.headers" -o /dev/null -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode "code=$pair_code" --data-urlencode 'name=OpenCode smoke' "$REMOTE_URL/fern/pair"
device_cookie=$(sed -nE 's/^Set-Cookie: __Host-fern_device=([^;]+).*/\1/p' "$RUN_ROOT/pair.headers" | tr -d '\r')
[[ -n "$device_cookie" ]]
curl -fsS -H "Cookie: __Host-fern_device=$device_cookie" "$REMOTE_URL/" >/dev/null

# Exercise the authenticated browser entry point, not just JSON APIs.
curl -fsS -u "opencode:$PASSWORD" -D "$RUN_ROOT/root.headers" -o "$RUN_ROOT/root.html" "$OPERATOR_URL/"
grep -qi '^content-type: *text/html' "$RUN_ROOT/root.headers"
grep -Eqi '<!doctype html|<html[ >]' "$RUN_ROOT/root.html"
[[ $(curl -sS -o /dev/null -w '%{http_code}' "$OPERATOR_URL/") == 401 ]]

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
  curl -fsS -u "opencode:$PASSWORD" -o "$RUN_ROOT/ui-asset" "$OPERATOR_URL$asset"
  [[ -s "$RUN_ROOT/ui-asset" ]]
else
  grep -Eqi '<(script|style)([ >])[^<]+' "$RUN_ROOT/root.html"
fi

curl -fsS -u "opencode:$PASSWORD" -D "$RUN_ROOT/spa.headers" \
  -o "$RUN_ROOT/spa.html" "$OPERATOR_URL/fern-smoke/direct-navigation"
grep -qi '^content-type: *text/html' "$RUN_ROOT/spa.headers"
grep -Eqi '<!doctype html|<html[ >]' "$RUN_ROOT/spa.html"

set +e
curl -sS -N --max-time 2 -u "opencode:$PASSWORD" "$OPERATOR_URL/api/event" >"$RUN_ROOT/events.txt"
event_status=$?
set -e
[[ "$event_status" == 28 ]]
grep -q '"type":"server.connected"' "$RUN_ROOT/events.txt"

session=$(curl -fsS -u "opencode:$PASSWORD" -H 'Content-Type: application/json' -d '{}' "$OPERATOR_URL/api/session")
session_id=$(jq -er '.data.id' <<<"$session")
curl -fsS -u "opencode:$PASSWORD" -H 'Content-Type: application/json' \
  -d '{"command":"sleep 3","timeout":0,"metadata":{}}' "$OPERATOR_URL/api/shell" >/dev/null
[[ $(curl -fsS -u "opencode:$PASSWORD" "$OPERATOR_URL/api/shell" | jq '.data | length') == 1 ]]
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
curl -fsS -u "opencode:$PASSWORD" "$OPERATOR_URL/api/session/$session_id" | jq -e ".data.id == \"$session_id\"" >/dev/null
stop_fern

echo "OpenCode smoke test passed: web UI, SPA fallback, auth, health, SSE, shell activity, recreation, session persistence, and config persistence"
