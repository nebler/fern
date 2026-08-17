#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUN_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/fern-browser.XXXXXX")
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
NAME="fern-browser-$PORT"
IMAGE=${FERN_BROWSER_IMAGE:-"fern/browser-smoke:$PORT"}
SESSION="fern-browser-$PORT"
URL="http://127.0.0.1:$PORT"
PASSWORD="browser-$PORT-secret"
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
  listen: 127.0.0.1:$PORT
EOF

start_fern() {
  HOME="$RUN_ROOT/home" OPENCODE_PASSWORD="$PASSWORD" "$RUN_ROOT/fern" up -config "$RUN_ROOT/fern.yaml" >>"$RUN_ROOT/fern.log" 2>&1 &
  FERN_PID=$!
  for _ in {1..350}; do
    if curl -fsS --user "opencode:$PASSWORD" "$URL/fern/ready" >/dev/null 2>&1; then return; fi
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
PAIR_CODE=$(curl -fsS --user "opencode:$PASSWORD" -X POST "$URL/fern/pair/new" | jq -er .code)
if [[ -n ${FERN_BROWSER_ENGINE:-} ]]; then
  playwright-cli -s="$SESSION" open --browser="$FERN_BROWSER_ENGINE" "$URL/fern/pair?code=$PAIR_CODE&name=Automated%20Mobile%20Browser" >/dev/null
else
  playwright-cli -s="$SESSION" open "$URL/fern/pair?code=$PAIR_CODE&name=Automated%20Mobile%20Browser" >/dev/null
fi
playwright-cli -s="$SESSION" resize 390 844 >/dev/null
playwright-cli -s="$SESSION" run-code "async page => {
  if (page.url() !== '$URL/fern/') throw new Error('pair redirect failed: ' + page.url());
  const cookies = await page.context().cookies();
  const device = cookies.find(cookie => cookie.name === 'fern_device');
  if (!device || !device.httpOnly || !device.secure || device.sameSite !== 'Strict' || device.path !== '/') throw new Error('invalid device cookie');
  if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error('mobile horizontal overflow');
  await page.getByRole('textbox', {name: 'Workflow title'}).fill('Automated mobile workflow');
  await page.getByRole('textbox', {name: 'OpenCode session ID'}).fill('ses_browser_rehearsal');
  await page.getByRole('button', {name: 'Track OpenCode session'}).click();
  await page.getByText('Automated mobile workflow').waitFor();
  if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error('workflow caused mobile overflow');
}" >/dev/null

REPLAY_STATUS=$(curl -sS -o /dev/null -w '%{http_code}' "$URL/fern/pair?code=$PAIR_CODE")
[[ "$REPLAY_STATUS" == 401 ]] || { echo "pairing code replay returned $REPLAY_STATUS" >&2; exit 1; }

stop_fern
start_fern
playwright-cli -s="$SESSION" reload >/dev/null
playwright-cli -s="$SESSION" run-code "async page => {
  await page.getByText('Automated mobile workflow').waitFor();
  await page.getByText('Automated Mobile Browser').waitFor();
  const response = await page.request.get('$URL/fern/api/v1/workflows');
  if (response.status() !== 200) throw new Error('device cookie did not survive Fern restart');
  await page.getByRole('button', {name: 'Revoke'}).click();
  await page.getByRole('heading', {name: 'Device revoked'}).waitFor();
  const revoked = await page.request.get('$URL/fern/ready');
  if (revoked.status() !== 401) throw new Error('revoked device still authenticates');
}" >/dev/null

printf 'Fern mobile browser rehearsal passed (390x844, restart, workflow, revocation)\n'
