#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-$RANDOM"
NAME="fern-v2-smoke-${RUN_ID//[^A-Za-z0-9_.-]/}"
VOLUME="fern-$NAME-v2-data"
RUN_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/fern-v2-smoke.XXXXXX")
REPO="$RUN_ROOT/repository"
CONFIG="$RUN_ROOT/fern.yaml"
FERN_BIN=${FERN_BIN:-}
IMAGE=${FERN_V2_IMAGE:-"fern/opencode-v2:smoke-$RUN_ID"}
PASSWORD="v2-smoke-$RUN_ID"
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

if [[ -z ${FERN_V2_IMAGE:-} ]]; then
  docker build -t "$IMAGE" "$ROOT/images/opencode-v2"
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
  opencode: v2
  repo: $REPO
  memory: 512Mi
idle:
  after: 1h
proxy:
  listen: 127.0.0.1:$PROXY_PORT
EOF

export OPENCODE_PASSWORD="$PASSWORD"
unset OPENCODE_SERVER_USERNAME OPENCODE_SERVER_PASSWORD

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
  echo "V2 Fern proxy did not become healthy" >&2
  return 1
}

stop_fern() {
  kill -TERM "$FERN_PID"
  wait "$FERN_PID"
  FERN_PID=""
}

start_fern
jq -e '.healthy == true and .version == "0.0.0-next-17444"' "$RUN_ROOT/health.json" >/dev/null
[[ $(curl -sS -o /dev/null -w '%{http_code}' "$PROXY_URL/api/health") == 401 ]]

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

docker exec -e "OPENCODE_PASSWORD=$PASSWORD" "$NAME" \
  opencode2 api --server http://127.0.0.1:4096 get /api/health | jq -e '.healthy == true' >/dev/null

stop_fern
"$FERN_BIN" down -config "$CONFIG"
docker volume inspect "$VOLUME" >/dev/null
start_fern
curl -fsS -u "opencode:$PASSWORD" "$PROXY_URL/api/session/$session_id" | jq -e ".data.id == \"$session_id\"" >/dev/null
stop_fern

echo "OpenCode V2 smoke test passed: auth, health, SSE, shell activity, official client, and persistence"
