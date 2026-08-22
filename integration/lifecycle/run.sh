#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-$RANDOM"
SAFE_ID=$(printf '%s' "$RUN_ID" | tr -cd 'A-Za-z0-9_.-')
NAME="fern-it-$SAFE_ID"
BLOCKER="fern-it-blocker-$SAFE_ID"
VOLUME="fern-$NAME-v2-data"
HEALTH_PATH=/api/health
USERNAME=opencode
IMAGE=${FERN_LIFECYCLE_IMAGE:-"fern/lifecycle:$SAFE_ID"}
ARTIFACTS=${FERN_LIFECYCLE_ARTIFACTS:-"$ROOT/integration/artifacts/$RUN_ID"}
RUN_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/fern-lifecycle.XXXXXX")
HOME_DIR="$RUN_ROOT/home"
REPO_DIR="$RUN_ROOT/repository"
CONFIG="$RUN_ROOT/fern.yaml"
FERN_LOG="$ARTIFACTS/fern.log"
FERN_RAW_LOG="$RUN_ROOT/fern.log"
TRANSCRIPT="$ARTIFACTS/transcript.log"
EVENTS="$ARTIFACTS/docker-events.log"
TIMINGS="$ARTIFACTS/wake-timings.tsv"
PASSWORD="lifecycle-$SAFE_ID-secret"
KEEP=${FERN_LIFECYCLE_KEEP_RESOURCES:-0}
WAKE_COUNT=${FERN_LIFECYCLE_WAKE_COUNT:-10}
FERN_BIN=${FERN_BIN:-}
FERN_PID=""
EVENTS_PID=""
BUILT_IMAGE=0
BLOCKER_STARTED=0
FAILED=0
REMOTE_PORT=""
OPERATOR_PORT=""
REMOTE_URL=""
OPERATOR_URL=""
REMOTE_ORIGIN="https://remote.lifecycle.invalid"

mkdir -p "$ARTIFACTS" "$HOME_DIR" "$REPO_DIR" "$RUN_ROOT/bin" \
  "$RUN_ROOT/cache" "$RUN_ROOT/config" "$RUN_ROOT/data"
: >"$TRANSCRIPT"
: >"$FERN_LOG"
: >"$FERN_RAW_LOG"
: >"$EVENTS"

redact() {
  sed -E \
    -e "s/${PASSWORD//\//\\/}/[REDACTED]/g" \
    -e 's/((PASSWORD|TOKEN|API_KEY|AUTHORIZATION)[=:][[:space:]]*)[^,[:space:]"}]+/\1[REDACTED]/Ig' \
    -e 's/(Authorization: Basic )[A-Za-z0-9+\/=]+/\1[REDACTED]/Ig'
}

timestamp() {
  python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))' 2>/dev/null \
    || date -u +%Y-%m-%dT%H:%M:%SZ
}

note() {
  printf '%s %s\n' "$(timestamp)" "$*" | tee -a "$TRANSCRIPT"
}

fail() {
  FAILED=1
  note "FAIL: $*"
  return 1
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

record_command() {
  printf '%s COMMAND' "$(timestamp)" >>"$TRANSCRIPT"
  printf ' %q' "$@" | redact >>"$TRANSCRIPT"
  printf '\n' >>"$TRANSCRIPT"
}

run_transcript() {
  local result
  record_command "$@"
  set +e
  "$@" >>"$TRANSCRIPT" 2>&1
  result=$?
  set -e
  note "EXIT status=$result command=$1"
  return "$result"
}

docker_exists() { docker container inspect "$1" >/dev/null 2>&1; }

capture_diagnostics() {
  set +e
  note "capturing diagnostics"
  {
    printf 'run_id=%s\nworkspace=%s\ncontainer=%s\nvolume=%s\nimage=%s\nproxy=%s\n' \
      "$RUN_ID" "$RUN_ROOT" "$NAME" "$VOLUME" "$IMAGE" "$REMOTE_URL (remote), $OPERATOR_URL (operator)"
    printf 'provider_backed=NOT_RUN (non-deterministic credentials, network, and billing required)\n'
  } >"$ARTIFACTS/run.txt"
  docker container inspect "$NAME" 2>&1 | redact >"$ARTIFACTS/container-inspect.json"
  docker volume inspect "$VOLUME" 2>&1 | redact >"$ARTIFACTS/volume-inspect.json"
  docker logs --timestamps "$NAME" 2>&1 | redact >"$ARTIFACTS/container.log"
  if [[ -f "$FERN_RAW_LOG" ]]; then redact <"$FERN_RAW_LOG" >"$FERN_LOG"; fi
  docker stats --no-stream "$NAME" 2>&1 | redact >"$ARTIFACTS/docker-stats.txt"
  { uname -a; command -v vm_stat >/dev/null && vm_stat; command -v free >/dev/null && free -h; } \
    >"$ARTIFACTS/host-memory.txt" 2>&1
  if [[ -x "$FERN_BIN" ]]; then "$FERN_BIN" status -name "$NAME" 2>&1 | redact >"$ARTIFACTS/fern-status.txt"; fi
  if [[ -n "$FERN_PID" ]]; then ps -o pid=,ppid=,state=,etime=,command= -p "$FERN_PID" 2>&1 | redact >"$ARTIFACTS/fern-process.txt"; fi
  set -e
}

stop_fern() {
  local result
  if [[ -n "$FERN_PID" ]] && kill -0 "$FERN_PID" 2>/dev/null; then
    note "sending SIGTERM to Fern pid=$FERN_PID"
    kill -TERM "$FERN_PID" 2>/dev/null || true
    for _ in {1..100}; do kill -0 "$FERN_PID" 2>/dev/null || break; sleep 0.05; done
    if kill -0 "$FERN_PID" 2>/dev/null; then
      kill -KILL "$FERN_PID" 2>/dev/null || true
      fail "Fern did not exit within 5 seconds of SIGTERM" || true
    fi
    set +e
    wait "$FERN_PID" 2>/dev/null
    result=$?
    set -e
    note "EXIT status=$result command=fern-up"
  fi
  FERN_PID=""
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if (( status != 0 )); then FAILED=1; fi
  capture_diagnostics
  # Keep mode retains evidence and Docker state, never host listeners or helper
  # processes that can interfere with later runs.
  stop_fern || true
  if [[ -n "$EVENTS_PID" ]]; then kill "$EVENTS_PID" 2>/dev/null || true; wait "$EVENTS_PID" 2>/dev/null || true; fi
  if [[ "$KEEP" == "1" ]]; then
    note "resources retained by request: home=$HOME_DIR container=$NAME volume=$VOLUME image=$IMAGE"
  else
    docker rm -f "$BLOCKER" >/dev/null 2>&1 || true
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    docker volume rm "$VOLUME" >/dev/null 2>&1 || true
    if [[ "$BUILT_IMAGE" == "1" ]]; then docker image rm "$IMAGE" >/dev/null 2>&1 || true; fi
    # The Go module cache intentionally makes downloaded modules read-only.
    # Restore owner write permission so isolated harness state remains removable.
    chmod -R u+w "$RUN_ROOT" 2>/dev/null || true
    rm -rf "$RUN_ROOT"
    if docker_exists "$NAME" || docker_exists "$BLOCKER" || docker volume inspect "$VOLUME" >/dev/null 2>&1; then
      note "FAIL: cleanup left an exact harness resource"
      FAILED=1
    fi
  fi
  if (( FAILED != 0 || status != 0 )); then
    note "lifecycle harness failed; redacted evidence: $ARTIFACTS"
    exit 1
  fi
  note "lifecycle harness passed; evidence: $ARTIFACTS"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

require_command() { command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"; }
for command in docker curl go python3 jq sed awk grep date ps tar; do require_command "$command"; done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  fail "missing required command: sha256sum or shasum"
fi
[[ "$WAKE_COUNT" =~ ^[0-9]+$ ]] && (( WAKE_COUNT >= 10 )) || fail "FERN_LIFECYCLE_WAKE_COUNT must be at least 10"
docker info >/dev/null 2>"$ARTIFACTS/docker-info-error.log" || fail "Docker daemon is unavailable; see $ARTIFACTS/docker-info-error.log"

export HOME="$HOME_DIR"
export XDG_CONFIG_HOME="$RUN_ROOT/config"
export XDG_CACHE_HOME="$RUN_ROOT/cache"
export XDG_DATA_HOME="$RUN_ROOT/data"
export GOCACHE="$RUN_ROOT/cache/go-build"
export GOPATH="$RUN_ROOT/go"
export OPENCODE_PASSWORD="$PASSWORD"
export FERN_CONTROL_PASSWORD="control-$PASSWORD"
unset ANTHROPIC_API_KEY OPENAI_API_KEY AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN

if [[ -n ${FERN_BIN:-} ]]; then
  [[ -x "$FERN_BIN" ]] || fail "FERN_BIN is not executable: $FERN_BIN"
  FERN_BIN=$(cd "$(dirname "$FERN_BIN")" && pwd)/$(basename "$FERN_BIN")
  note "using explicit Fern binary $FERN_BIN"
else
  FERN_BIN="$RUN_ROOT/bin/fern"
  (cd "$ROOT" && run_transcript go build -o "$FERN_BIN" ./cmd/fern)
fi

if [[ -z ${FERN_LIFECYCLE_IMAGE:-} ]]; then
  run_transcript docker build -t "$IMAGE" "$ROOT/integration/lifecycle"
  BUILT_IMAGE=1
else
  docker image inspect "$IMAGE" >/dev/null || fail "explicit lifecycle image does not exist: $IMAGE"
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
printf 'fixture created by %s\n' "$RUN_ID" >"$REPO_DIR/fixture.txt"
chmod 0777 "$REPO_DIR"
cat >"$CONFIG" <<EOF
workspace:
  name: $NAME
  image: $IMAGE
  repo: $REPO_DIR
  memory: 128Mi
idle:
  after: 2s
proxy:
  listen: 127.0.0.1:$REMOTE_PORT
  operatorListen: 127.0.0.1:$OPERATOR_PORT
  remoteOrigin: $REMOTE_ORIGIN
control:
  password: \${FERN_CONTROL_PASSWORD}
EOF

docker events --filter "container=$NAME" --format '{{json .}}' >"$EVENTS" 2>&1 &
EVENTS_PID=$!

auth_curl() { curl --silent --show-error --user "$USERNAME:$PASSWORD" "$@"; }
control_curl() { curl --silent --show-error --user "fern:$FERN_CONTROL_PASSWORD" "$@"; }
http_code() {
  local output=$1; shift
  curl --silent --show-error --output "$output" --write-out '%{http_code}' "$@"
}

wait_status() {
  local wanted=$1
  local timeout=${2:-20}
  local line
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    line=$("$FERN_BIN" status -name "$NAME" 2>/dev/null || true)
    [[ "$line" == *$'\t'"$wanted"* ]] && return 0
    sleep 0.1
  done
  fail "workspace did not reach Fern state $wanted (last: ${line:-none})"
}

wait_http() {
  local deadline=$((SECONDS + 70)) code
  while (( SECONDS < deadline )); do
    code=$(http_code /dev/null --user "$USERNAME:$PASSWORD" "$OPERATOR_URL$HEALTH_PATH" 2>/dev/null || true)
    [[ "$code" == "200" ]] && return 0
    kill -0 "$FERN_PID" 2>/dev/null || fail "Fern exited before becoming ready"
    sleep 0.2
  done
  fail "proxy did not become healthy"
}

start_fern() {
	start_fern_control
	wait_http
}

start_fern_control() {
  printf '\n=== fern start %s ===\n' "$(timestamp)" >>"$FERN_RAW_LOG"
  record_command "$FERN_BIN" up -config "$CONFIG"
  "$FERN_BIN" up -config "$CONFIG" >>"$FERN_RAW_LOG" 2>&1 &
  FERN_PID=$!
	local deadline=$((SECONDS + 20)) code
	while (( SECONDS < deadline )); do
		code=$(http_code /dev/null --user "fern:$FERN_CONTROL_PASSWORD" "$OPERATOR_URL/fern/ready" 2>/dev/null || true)
		[[ "$code" == "200" ]] && return 0
		kill -0 "$FERN_PID" 2>/dev/null || fail "Fern exited before its control plane became ready"
		sleep 0.2
	done
	fail "Fern control plane did not become ready"
}

activity() {
  local session=${1:-lifecycle}
  auth_curl --fail --request POST "$OPERATOR_URL/control/activity?session=$session&delay=0.15" >/dev/null
}

stop_by_idle() {
  activity "idle-$RANDOM"
  wait_status paused 15
}

container_started_at() { docker inspect --format '{{.State.StartedAt}}' "$NAME"; }
container_id() { docker inspect --format '{{.Id}}' "$NAME"; }
endpoint() { docker port "$NAME" 4096/tcp | awk -F: 'NR==1 {print $NF}'; }

note "scenario 1/14: create and become healthy"
start_fern
wait_status running
initial_id=$(container_id)
[[ -n "$initial_id" ]] || fail "created container has no ID"

note "scenario 2/14: authorized request reaches the OpenCode protocol service"
identity=$(auth_curl --fail "$OPERATOR_URL/control/identity")
[[ "$identity" == *'"boot_id"'* ]] || fail "authorized response did not come from lifecycle service"

note "scenario 3/14: stopped requests reject missing/wrong credentials without wake"
stop_by_idle
before_start=$(container_started_at)
missing_code=$(http_code "$ARTIFACTS/missing-auth.body" "$REMOTE_URL$HEALTH_PATH" || true)
wrong_code=$(http_code "$ARTIFACTS/wrong-auth.body" --user "$USERNAME:wrong" "$REMOTE_URL$HEALTH_PATH" || true)
backend_code=$(http_code "$ARTIFACTS/backend-auth.body" --user "$USERNAME:$PASSWORD" "$REMOTE_URL$HEALTH_PATH" || true)
control_code=$(http_code "$ARTIFACTS/control-auth.body" --user "fern:$FERN_CONTROL_PASSWORD" "$REMOTE_URL$HEALTH_PATH" || true)
[[ "$missing_code" == 401 && "$wrong_code" == 401 && "$backend_code" == 401 && "$control_code" == 401 ]] \
  || fail "remote auth expected 401/401/401/401, got $missing_code/$wrong_code/$backend_code/$control_code"
	control_curl --fail "$OPERATOR_URL/fern/ready" | grep -q '"ready":true' || fail "Fern readiness was unavailable while compute was paused"
	pair_code=$(control_curl --fail --request POST "$OPERATOR_URL/fern/pair/new" | jq -er '.code')
pair_preview_code=$(http_code "$ARTIFACTS/pairing-preview.body" --dump-header "$ARTIFACTS/pairing-preview.headers" "$REMOTE_URL/fern/pair?code=$pair_code")
[[ "$pair_preview_code" == 200 ]] || fail "phone pairing preview returned HTTP $pair_preview_code"
grep -q 'Pair this phone' "$ARTIFACTS/pairing-preview.body" || fail "phone pairing preview did not render confirmation"
if grep -Eqi '^set-cookie: (__Host-)?fern_device=' "$ARTIFACTS/pairing-preview.headers"; then
  fail "phone pairing preview consumed the code and issued a device cookie"
fi
pair_confirm_code=$(http_code /dev/null --dump-header "$ARTIFACTS/pairing.headers" \
  --header 'Content-Type: application/x-www-form-urlencoded' --data-urlencode "code=$pair_code" "$REMOTE_URL/fern/pair")
[[ "$pair_confirm_code" == 303 ]] || fail "phone pairing confirmation returned HTTP $pair_confirm_code"
grep -Eqi '^location: /fern/' "$ARTIFACTS/pairing.headers" || fail "phone pairing confirmation did not redirect to the Fern landing page"
grep -Eqi '^set-cookie: __Host-fern_device=' "$ARTIFACTS/pairing.headers" \
  && grep -Eqi '^set-cookie: .*HttpOnly' "$ARTIFACTS/pairing.headers" \
  && grep -Eqi '^set-cookie: .*Secure' "$ARTIFACTS/pairing.headers" \
  || fail "phone pairing did not issue a secure HttpOnly cookie"
pair_replay_code=$(http_code /dev/null --header 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode "code=$pair_code" "$REMOTE_URL/fern/pair")
[[ "$pair_replay_code" == 401 ]] || fail "phone pairing replay returned HTTP $pair_replay_code"
sleep 0.5
wait_status paused 2
[[ "$(container_started_at)" == "$before_start" ]] || fail "authentication or Fern-owned routes started compute"
device_cookie=$(sed -nE 's/^Set-Cookie: __Host-fern_device=([^;]+).*/\1/p' "$ARTIFACTS/pairing.headers" | tr -d '\r')
paired_identity=$(curl --silent --show-error --fail --header "Cookie: __Host-fern_device=$device_cookie" "$REMOTE_URL/control/identity")
[[ "$paired_identity" == *'"boot_id"'* ]] || fail "paired device cookie did not authenticate and wake through Fern"
curl --silent --show-error --fail --dump-header "$ARTIFACTS/remote-forwarding.headers" \
  --header "Cookie: __Host-fern_device=$device_cookie" --header 'Host: malicious.example:9999' \
  --header 'Forwarded: for=attacker;proto=http' --header 'X-Forwarded-For: attacker' \
  --header 'X-Forwarded-Host: malicious.example' --header 'X-Forwarded-Proto: http' \
  --header 'X-Forwarded-Port: 1' --header 'X-Forwarded-Evil: retained' \
  "$REMOTE_URL/control/forwarding" >"$ARTIFACTS/remote-forwarding.json"
jq -e --arg host "remote.lifecycle.invalid" \
  '.host == $host and .x_forwarded_host == $host and .x_forwarded_proto == "https" and .x_forwarded_port == "443" and .forwarded == "" and .x_forwarded_for == "" and .x_forwarded_extension == ""' \
  "$ARTIFACTS/remote-forwarding.json" >/dev/null || fail "paired remote request did not receive canonical forwarding metadata"
grep -Eqi '^location: https://remote\.lifecycle\.invalid/generated-location' "$ARTIFACTS/remote-forwarding.headers" \
  && grep -Eqi '^link: <https://remote\.lifecycle\.invalid/generated-link>' "$ARTIFACTS/remote-forwarding.headers" \
  || fail "remote absolute response headers did not use the configured HTTPS origin"
curl --silent --show-error --fail --user "$USERNAME:$PASSWORD" --header 'Host: malicious.example' \
  --header 'Forwarded: for=attacker' --header 'X-Forwarded-Evil: retained' \
  "$OPERATOR_URL/control/forwarding" >"$ARTIFACTS/operator-forwarding.json"
jq -e --arg host "127.0.0.1:$OPERATOR_PORT" --arg port "$OPERATOR_PORT" \
  '.host == $host and .x_forwarded_host == $host and .x_forwarded_proto == "http" and .x_forwarded_port == $port and .forwarded == "" and .x_forwarded_for == "" and .x_forwarded_extension == ""' \
  "$ARTIFACTS/operator-forwarding.json" >/dev/null || fail "operator request did not receive canonical local forwarding metadata"
stop_by_idle

note "scenario 4/14: concurrent authorized requests coalesce into one wake"
starts_before=$(grep -c '"Action":"start"' "$EVENTS" || true)
pids=()
for index in {1..12}; do
  auth_curl --fail "$OPERATOR_URL$HEALTH_PATH" >"$ARTIFACTS/concurrent-$index.json" & pids+=("$!")
done
for pid in "${pids[@]}"; do wait "$pid" || fail "concurrent authorized request failed"; done
sleep 0.3
starts_after=$(grep -c '"Action":"start"' "$EVENTS" || true)
(( starts_after - starts_before == 1 )) || fail "concurrent wake emitted $((starts_after - starts_before)) Docker start events, expected 1"

note "scenario 5/14: busy-to-idle activity stops compute"
stop_by_idle

note "scenario 6/14: final phone work request restarts the full idle grace period"
phone_session="phone-idle-$SAFE_ID"
phone_marker="phone-state-$RUN_ID"
curl --silent --show-error --fail --max-time 5 --header "Cookie: __Host-fern_device=$device_cookie" \
  --request POST --header 'Content-Type: application/json' \
  --data "{\"marker\":\"$phone_marker\",\"session\":\"$phone_session\"}" \
  "$REMOTE_URL/control/persist" >/dev/null
curl --silent --show-error --fail --max-time 5 --header "Cookie: __Host-fern_device=$device_cookie" \
  --request POST "$REMOTE_URL/control/activity?session=$phone_session&delay=0.6" >/dev/null

active_sessions='{}'
activity_deadline=$((SECONDS + 5))
while (( SECONDS < activity_deadline )); do
  active_sessions=$(curl --silent --show-error --fail --max-time 2 --header "Cookie: __Host-fern_device=$device_cookie" \
    "$REMOTE_URL/api/session/active")
  jq -e --arg session "$phone_session" '.data[$session].type == "running"' <<<"$active_sessions" >/dev/null && break
  sleep 0.05
done
jq -e --arg session "$phone_session" '.data[$session].type == "running"' <<<"$active_sessions" >/dev/null \
  || fail "phone lifecycle session was not authoritatively observed busy"

activity_deadline=$((SECONDS + 5))
while (( SECONDS < activity_deadline )); do
  active_sessions=$(curl --silent --show-error --fail --max-time 2 --header "Cookie: __Host-fern_device=$device_cookie" \
    "$REMOTE_URL/api/session/active")
  jq -e --arg session "$phone_session" '.data[$session] == null' <<<"$active_sessions" >/dev/null && break
  sleep 0.05
done
jq -e --arg session "$phone_session" '.data[$session] == null' <<<"$active_sessions" >/dev/null \
  || fail "phone lifecycle session was not authoritatively observed idle"

# Spend part of the first grace period, then make the browser-like work request
# whose observation must replace that deadline with a complete new grace period.
sleep 1
phone_container_id=$(container_id)
phone_started_at=$(container_started_at)
phone_request_ns=$(python3 -c 'import time; print(time.monotonic_ns())')
phone_identity=$(curl --silent --show-error --fail --max-time 5 --header "Cookie: __Host-fern_device=$device_cookie" \
  "$REMOTE_URL/control/identity")
phone_boot_id=$(jq -er '.boot_id' <<<"$phone_identity")
sleep 1.3
[[ $(docker inspect --format '{{.State.Running}}' "$NAME") == true ]] \
  || fail "compute paused before the final phone request received a full restarted grace period"
[[ $(container_started_at) == "$phone_started_at" ]] \
  || fail "compute restarted while checking the final phone request grace period"

wait_status paused 5
phone_pause_ns=$(python3 -c 'import time; print(time.monotonic_ns())')
(( phone_pause_ns - phone_request_ns >= 1900000000 )) \
  || fail "compute paused before the restarted 2-second grace period elapsed"
phone_wake_identity=$(curl --silent --show-error --fail --max-time 70 --header "Cookie: __Host-fern_device=$device_cookie" \
  "$REMOTE_URL/control/identity")
[[ $(jq -er '.boot_id' <<<"$phone_wake_identity") != "$phone_boot_id" ]] \
  || fail "phone wake did not start a fresh backend process"
[[ $(container_id) == "$phone_container_id" ]] || fail "phone wake replaced the lifecycle container"
phone_state=$(curl --silent --show-error --fail --max-time 5 --header "Cookie: __Host-fern_device=$device_cookie" \
  "$REMOTE_URL/control/persist")
jq -e --arg marker "$phone_marker" --arg session "$phone_session" \
  '.marker == $marker and .session == $session' <<<"$phone_state" >/dev/null \
  || fail "phone wake did not preserve the OpenCode session state"
jq -e --arg marker "$phone_marker" --arg session "$phone_session" \
  '.marker == $marker and .session == $session' "$REPO_DIR/container-state.json" >/dev/null \
  || fail "phone wake did not preserve the repository state"

note "scenario 7/14: held request prevents stop"
auth_curl --fail "$OPERATOR_URL$HEALTH_PATH" >/dev/null
auth_curl --fail "$OPERATOR_URL/control/hold?seconds=4" >"$ARTIFACTS/held-request.json" &
hold_pid=$!
sleep 0.2
activity held
sleep 2.7
running=$(docker inspect --format '{{.State.Running}}' "$NAME")
[[ "$running" == true ]] || fail "compute stopped while an HTTP request was held"
wait "$hold_pid" || fail "held request failed"
wait_status paused 10

note "scenario 8/14: changed dynamic backend endpoint is discovered after stale failure"
auth_curl --fail "$OPERATOR_URL$HEALTH_PATH" >/dev/null
old_port=$(endpoint)
docker rm -f "$NAME" >/dev/null
docker run -d --name "$BLOCKER" --label "dev.fern.lifecycle=$RUN_ID" -e "OPENCODE_PASSWORD=$PASSWORD" -p "127.0.0.1:$old_port:4096" "$IMAGE" >/dev/null
BLOCKER_STARTED=1
first_code=$(http_code "$ARTIFACTS/stale-endpoint.body" --user "$USERNAME:$PASSWORD" --max-time 5 "$OPERATOR_URL$HEALTH_PATH" || true)
[[ "$first_code" == 502 || "$first_code" == 503 || "$first_code" == 000 ]] || fail "stale endpoint did not fail safely (HTTP $first_code)"
second_code=$(http_code "$ARTIFACTS/dynamic-endpoint.body" --user "$USERNAME:$PASSWORD" --max-time 70 "$OPERATOR_URL$HEALTH_PATH" || true)
[[ "$second_code" == 200 ]] || fail "request after stale endpoint did not wake replacement (HTTP $second_code)"
new_port=$(endpoint)
[[ "$new_port" != "$old_port" ]] || fail "backend endpoint did not change while old port was occupied"
docker rm -f "$BLOCKER" >/dev/null
BLOCKER_STARTED=0

note "scenario 9/14: repository and OpenCode data survive stop/start"
auth_curl --fail --request POST --header 'Content-Type: application/json' --data "{\"marker\":\"$RUN_ID\"}" "$OPERATOR_URL/control/persist" >/dev/null
stop_by_idle
auth_curl --fail "$OPERATOR_URL$HEALTH_PATH" >/dev/null
persisted=$(auth_curl --fail "$OPERATOR_URL/control/persist")
[[ "$persisted" == *"$RUN_ID"* ]] || fail "OpenCode data did not survive stop/start"
grep -q "$RUN_ID" "$REPO_DIR/container-state.json" || fail "container-written repository data did not survive stop/start"

note "scenario 10/14: isolated backup and destructive restore"
workflow_id=$(control_curl --fail --request POST --header 'Content-Type: application/json' \
  --data '{"title":"Lifecycle workflow","sessionId":"ses_lifecycle"}' \
  "$OPERATOR_URL/fern/api/v1/workflows" | jq -er '.id')
stop_fern
run_transcript "$FERN_BIN" down -name "$NAME"
[[ ! $(docker ps -aq --filter "name=^/${NAME}$") ]] || fail "down did not remove compute"
docker volume inspect "$VOLUME" >/dev/null || fail "down removed the persistent data volume"

BACKUP_DIR="$RUN_ROOT/backup"
mkdir -p "$BACKUP_DIR"
tar -C "$RUN_ROOT" -czf "$BACKUP_DIR/repository.tar.gz" repository
tar -C "$HOME_DIR" -czf "$BACKUP_DIR/fern-control.tar.gz" .fern/control
tar -C "$RUN_ROOT" -czf "$BACKUP_DIR/config.tar.gz" fern.yaml
docker run --rm --user 0:0 --entrypoint sh \
  -v "$VOLUME:/source:ro" -v "$BACKUP_DIR:/backup" "$IMAGE" \
  -c 'tar -C /source -czf /backup/opencode-volume.tar.gz .'
(cd "$BACKUP_DIR" && sha256 repository.tar.gz fern-control.tar.gz config.tar.gz opencode-volume.tar.gz >SHA256SUMS)
while read -r expected archive; do
  actual=$(sha256 "$BACKUP_DIR/$archive" | awk '{print $1}')
  [[ "$actual" == "$expected" ]] || fail "backup checksum failed for $archive"
done <"$BACKUP_DIR/SHA256SUMS"
cp "$BACKUP_DIR/fern-control.tar.gz" "$BACKUP_DIR/corrupt.tar.gz"
printf 'corrupt' >>"$BACKUP_DIR/corrupt.tar.gz"
expected_control=$(awk '$2 == "fern-control.tar.gz" {print $1}' "$BACKUP_DIR/SHA256SUMS")
[[ "$(sha256 "$BACKUP_DIR/corrupt.tar.gz" | awk '{print $1}')" != "$expected_control" ]] \
  || fail "corrupt backup was not detected"

rm -rf "$REPO_DIR" "$HOME_DIR/.fern" "$CONFIG"
docker volume rm "$VOLUME" >/dev/null
docker volume create --label dev.fern.managed=true --label "dev.fern.workspace=$NAME" "$VOLUME" >/dev/null
tar -C "$RUN_ROOT" -xzf "$BACKUP_DIR/repository.tar.gz"
tar -C "$HOME_DIR" -xzf "$BACKUP_DIR/fern-control.tar.gz"
tar -C "$RUN_ROOT" -xzf "$BACKUP_DIR/config.tar.gz"
docker run --rm --user 0:0 --entrypoint sh \
  -v "$VOLUME:/target" -v "$BACKUP_DIR:/backup:ro" "$IMAGE" \
  -c 'tar -C /target -xzf /backup/opencode-volume.tar.gz'

start_fern
curl --silent --show-error --fail --header "Cookie: __Host-fern_device=$device_cookie" \
  "$REMOTE_URL/fern/" | grep -q 'href="/"' \
  || fail "paired device cookie did not survive Fern restart"
control_curl --fail "$OPERATOR_URL/fern/api/v1/devices" | jq -e 'length == 1' >/dev/null \
  || fail "durable paired device was not listed after restart"
control_curl --fail "$OPERATOR_URL/fern/api/v1/workflows/$workflow_id" | jq -e --arg id "$workflow_id" '.id == $id and .sessionId == "ses_lifecycle"' >/dev/null \
  || fail "durable workflow correlation did not survive Fern restart"
persisted=$(auth_curl --fail "$OPERATOR_URL/control/persist")
[[ "$persisted" == *"$RUN_ID"* ]] || fail "OpenCode volume content did not survive destructive restore"
grep -q "$RUN_ID" "$REPO_DIR/container-state.json" || fail "repository content did not survive destructive restore"

note "measurement: $WAKE_COUNT stopped-to-ready wakes"
printf 'iteration\trequest_time_utc\tcontainer_start_observed_utc\tdocker_started_at\thealth_ready_utc\twatcher_connected_ns\tfirst_upstream_byte_s\ttotal_s\tcontainer_id\tendpoint\tclassification\n' >"$TIMINGS"
for ((iteration=1; iteration<=WAKE_COUNT; iteration++)); do
  stop_by_idle
  request_time=$(timestamp)
  timing_file="$RUN_ROOT/timing-$iteration"
  body_file="$RUN_ROOT/body-$iteration"
  auth_curl --output "$body_file" --write-out '%{http_code}\t%{time_starttransfer}\t%{time_total}\n' "$OPERATOR_URL$HEALTH_PATH" >"$timing_file" &
  wake_pid=$!
  start_observed="unobservable"
  for _ in {1..700}; do
    if [[ $(docker inspect --format '{{.State.Running}}' "$NAME" 2>/dev/null || true) == true ]]; then start_observed=$(timestamp); break; fi
    kill -0 "$wake_pid" 2>/dev/null || break
    sleep 0.02
  done
  wait "$wake_pid" || fail "measured wake $iteration request failed"
  health_ready=$(timestamp)
  IFS=$'\t' read -r code first_byte total <"$timing_file"
  [[ "$code" == 200 ]] || fail "measured wake $iteration returned HTTP $code"
  id=$(container_id)
  port=$(endpoint)
  started_at=$(container_started_at)
  watcher_ns=$(docker logs "$NAME" 2>&1 | sed -n 's/.*event_connected ts_ns=\([0-9]*\).*/\1/p' | tail -n 1)
  [[ -n "$watcher_ns" && "$watcher_ns" != 0 ]] || watcher_ns=unobservable
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t127.0.0.1:%s\tready\n' \
    "$iteration" "$request_time" "$start_observed" "$started_at" "$health_ready" "$watcher_ns" "$first_byte" "$total" "$id" "$port" >>"$TIMINGS"
done

note "scenario 11/14: external clean exit is classified failed"
auth_curl --fail --request POST "$OPERATOR_URL/control/exit" >/dev/null
wait_status failed 10
"$FERN_BIN" status -name "$NAME" | grep -q $'\tfailed\t.*exit=0 oom=false' || fail "clean exit was not classified failed"
stop_fern
run_transcript "$FERN_BIN" down -name "$NAME"
start_fern

note "scenario 12/14: OOM is classified failed"
auth_curl --fail --request POST "$OPERATOR_URL/control/oom" >/dev/null
wait_status failed 20
"$FERN_BIN" status -name "$NAME" | grep -q $'\tfailed\t.*oom=true' || fail "reproducible cgroup OOM was not classified failed"
stop_fern
run_transcript "$FERN_BIN" down -name "$NAME"
start_fern

note "scenario 13/14: SIGTERM shuts Fern down without host-process/listener leaks"
stop_fern
if curl --silent --max-time 1 "$REMOTE_URL$HEALTH_PATH" >/dev/null 2>&1 || curl --silent --max-time 1 "$OPERATOR_URL$HEALTH_PATH" >/dev/null 2>&1; then fail "a proxy listener remained after SIGTERM"; fi
[[ -z "$FERN_PID" ]] || fail "Fern process remained after SIGTERM"
docker stop "$NAME" >/dev/null
wait_status paused 5
# Docker Desktop can retain the old dynamic port forwarding briefly after stop;
# a host reboot naturally has a much larger separation before Fern restarts.
sleep 1
start_fern_control
wait_status paused 10
auth_curl --fail "$OPERATOR_URL$HEALTH_PATH" >/dev/null
wait_status running 10

note "scenario 14/14: externally paused compute follows stale-endpoint recovery path"
docker pause "$NAME" >/dev/null
paused_code=$(http_code "$ARTIFACTS/paused-endpoint.body" --user "$USERNAME:$PASSWORD" --max-time 3 "$OPERATOR_URL$HEALTH_PATH" || true)
[[ "$paused_code" == 502 || "$paused_code" == 503 || "$paused_code" == 000 ]] || fail "paused stale endpoint did not fail safely (HTTP $paused_code)"
recovered_code=$(http_code "$ARTIFACTS/paused-recovery.body" --user "$USERNAME:$PASSWORD" --max-time 70 "$OPERATOR_URL$HEALTH_PATH" || true)
[[ "$recovered_code" == 200 ]] || fail "paused compute did not recover (HTTP $recovered_code)"
wait_status running

note "capturing active and stopped resource measurements"
docker stats --no-stream "$NAME" | redact >"$ARTIFACTS/docker-stats-active.txt"
stop_by_idle
docker stats --no-stream "$NAME" | redact >"$ARTIFACTS/docker-stats-stopped.txt"
note "all 14 deterministic scenarios passed; provider-backed scenarios NOT RUN by design"
