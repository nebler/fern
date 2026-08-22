#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUN_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/fern-browser.XXXXXX")
REMOTE_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
OPERATOR_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
NAME="fern-browser-$REMOTE_PORT"
IMAGE=${FERN_BROWSER_IMAGE:-"fern/browser-smoke:$REMOTE_PORT"}
SESSION="fern-browser-$REMOTE_PORT"
REMOTE_URL="http://127.0.0.1:$REMOTE_PORT"
OPERATOR_URL="http://127.0.0.1:$OPERATOR_PORT"
PASSWORD="browser-$REMOTE_PORT-secret"
CONTROL_PASSWORD="browser-$REMOTE_PORT-control-secret-control-secret"
FERN_PID=""
BUILT_IMAGE=0

cleanup() {
  set +e
  playwright-cli -s="$SESSION" close >/dev/null 2>&1
  if [[ -n "$FERN_PID" ]] && kill -0 "$FERN_PID" 2>/dev/null; then
    kill -TERM "$FERN_PID" 2>/dev/null
    wait "$FERN_PID" 2>/dev/null
  fi
  docker rm -f "$NAME" >/dev/null 2>&1
  docker volume rm "fern-$NAME-v2-data" >/dev/null 2>&1
  if [[ "$BUILT_IMAGE" == 1 ]]; then docker image rm "$IMAGE" >/dev/null 2>&1; fi
  rm -rf "$RUN_ROOT"
}
trap cleanup EXIT INT TERM

for command in curl docker go jq playwright-cli python3; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done
docker info >/dev/null
mkdir -p "$RUN_ROOT/home" "$RUN_ROOT/repository"
chmod 0777 "$RUN_ROOT/repository"
printf 'browser fixture\n' >"$RUN_ROOT/repository/README.md"
GOTOOLCHAIN=local go build -o "$RUN_ROOT/fern" "$ROOT/cmd/fern"
if [[ -z ${FERN_BROWSER_IMAGE:-} ]]; then
  docker build -q -t "$IMAGE" "$ROOT/integration/lifecycle" >/dev/null
  BUILT_IMAGE=1
fi
cat >"$RUN_ROOT/fern.yaml" <<EOF
workspace:
  name: $NAME
  image: $IMAGE
  repo: $RUN_ROOT/repository
  memory: 128Mi
idle:
  after: 30m
proxy:
  listen: 127.0.0.1:$REMOTE_PORT
  operatorListen: 127.0.0.1:$OPERATOR_PORT
  remoteOrigin: https://browser-smoke.invalid
control:
  password: \${FERN_CONTROL_PASSWORD}
EOF

start_fern() {
  HOME="$RUN_ROOT/home" OPENCODE_PASSWORD="$PASSWORD" FERN_CONTROL_PASSWORD="$CONTROL_PASSWORD" "$RUN_ROOT/fern" up -config "$RUN_ROOT/fern.yaml" >>"$RUN_ROOT/fern.log" 2>&1 &
  FERN_PID=$!
  for _ in {1..350}; do
    if curl -fsS --user "fern:$CONTROL_PASSWORD" "$OPERATOR_URL/fern/ready" >/dev/null 2>&1; then return; fi
    kill -0 "$FERN_PID" 2>/dev/null || { cat "$RUN_ROOT/fern.log" >&2; return 1; }
    sleep .2
  done
  echo "Fern browser fixture did not become ready" >&2
  return 1
}

stop_fern() {
  kill -TERM "$FERN_PID"
  wait "$FERN_PID"
  FERN_PID=""
}

start_fern
[[ $(curl -sS -o /dev/null -w '%{http_code}' --user "opencode:$PASSWORD" "$REMOTE_URL/api/health") == 401 ]]
[[ $(curl -sS -o /dev/null -w '%{http_code}' --user "fern:$CONTROL_PASSWORD" "$REMOTE_URL/api/health") == 401 ]]
PAIR_CODE=$(curl -fsS --user "fern:$CONTROL_PASSWORD" -X POST "$OPERATOR_URL/fern/pair/new" | jq -er .code)
PREVIEW_STATUS=$(curl -sS --dump-header "$RUN_ROOT/pair-preview.headers" --output "$RUN_ROOT/pair-preview.html" --write-out '%{http_code}' \
  "$REMOTE_URL/fern/pair?code=$PAIR_CODE")
[[ "$PREVIEW_STATUS" == 200 ]] || { echo "pairing preview returned $PREVIEW_STATUS" >&2; exit 1; }
grep -q 'Pair this phone' "$RUN_ROOT/pair-preview.html" || { echo "pairing preview did not render confirmation" >&2; exit 1; }
if grep -Eqi '^set-cookie: (__Host-)?fern_device=' "$RUN_ROOT/pair-preview.headers"; then
  echo "pairing preview consumed the code and issued a device cookie" >&2
  exit 1
fi
if [[ -n ${FERN_BROWSER_ENGINE:-} ]]; then
  playwright-cli -s="$SESSION" open --browser="$FERN_BROWSER_ENGINE" "$REMOTE_URL/fern/pair?code=$PAIR_CODE&name=Automated%20Mobile%20Browser" >/dev/null
else
  playwright-cli -s="$SESSION" open "$REMOTE_URL/fern/pair?code=$PAIR_CODE&name=Automated%20Mobile%20Browser" >/dev/null
fi
playwright-cli -s="$SESSION" resize 390 844 >/dev/null
playwright-cli -s="$SESSION" run-code "async page => {
  if (!page.url().startsWith('$REMOTE_URL/fern/pair?')) throw new Error('pair preview URL failed: ' + page.url());
  if (!await page.getByRole('heading', {name: 'Pair this phone?'}).count()) throw new Error('pair confirmation was not rendered');
  if ((await page.context().cookies()).some(cookie => cookie.name === '__Host-fern_device' || cookie.name === 'fern_device')) throw new Error('pair preview issued a device cookie');
  await Promise.all([
    page.waitForURL('$REMOTE_URL/fern/'),
    page.getByRole('button', {name: 'Pair this phone'}).click(),
  ]);
  if (page.url() !== '$REMOTE_URL/fern/') throw new Error('pair redirect failed: ' + page.url());
  const cookies = await page.context().cookies();
  const device = cookies.find(cookie => cookie.name === '__Host-fern_device');
  if (!device || !device.httpOnly || !device.secure || device.sameSite !== 'Strict' || device.path !== '/') throw new Error('invalid device cookie');
  if (cookies.some(cookie => cookie.name === 'fern_device')) throw new Error('legacy device cookie was issued');
  if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error('mobile horizontal overflow');
  if (await page.getByRole('textbox', {name: 'Workflow title'}).count()) throw new Error('paired device received Fern administration controls');
  const denied = await page.request.get('$REMOTE_URL/fern/api/v1/devices');
  if (denied.status() !== 404) throw new Error('paired cookie received Fern admin route');
}" >/dev/null

curl -fsS --user "fern:$CONTROL_PASSWORD" -H 'Content-Type: application/json' \
  -d '{"title":"Automated mobile workflow","sessionId":"ses_browser_rehearsal"}' \
  "$OPERATOR_URL/fern/api/v1/workflows" >/dev/null

REPLAY_STATUS=$(curl -sS -o /dev/null -w '%{http_code}' -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode "code=$PAIR_CODE" "$REMOTE_URL/fern/pair")
[[ "$REPLAY_STATUS" == 401 ]] || { echo "pairing code replay returned $REPLAY_STATUS" >&2; exit 1; }

stop_fern
start_fern
playwright-cli -s="$SESSION" reload >/dev/null
playwright-cli -s="$SESSION" run-code "async page => {
  const response = await page.request.get('$REMOTE_URL/fern/');
  if (response.status() !== 200) throw new Error('device cookie did not survive Fern restart');
}" >/dev/null

DEVICE_ID=$(curl -fsS --user "fern:$CONTROL_PASSWORD" "$OPERATOR_URL/fern/api/v1/devices" | jq -er '.[0].id')
curl -fsS --user "fern:$CONTROL_PASSWORD" -X DELETE "$OPERATOR_URL/fern/api/v1/devices/$DEVICE_ID" >/dev/null
playwright-cli -s="$SESSION" run-code "async page => {
  const revoked = await page.request.get('$REMOTE_URL/fern/');
  if (revoked.status() !== 401) throw new Error('revoked device still authenticates');
}" >/dev/null

printf 'Fern mobile browser rehearsal passed (390x844, scoped device, restart, admin revocation)\n'
