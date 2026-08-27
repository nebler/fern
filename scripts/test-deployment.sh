#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
TEMP=$(mktemp -d "${TMPDIR:-/tmp}/fern-deployment.XXXXXX")
trap 'rm -rf "$TEMP"' EXIT

UNIT="$ROOT/deploy/systemd/fern.service"
CONFIG="$ROOT/deploy/systemd/fern.yaml.example"
ENV_FILE="$ROOT/deploy/systemd/fern.env.example"

grep -qx 'User=fern' "$UNIT"
grep -qx 'Group=fern' "$UNIT"
grep -qx 'SupplementaryGroups=docker' "$UNIT"
grep -qx 'Environment=HOME=/var/lib/fern' "$UNIT"
grep -qx 'EnvironmentFile=/etc/fern/fern.env' "$UNIT"
grep -qx 'UMask=0077' "$UNIT"
grep -qx 'PrivateTmp=true' "$UNIT"
grep -qx 'NoNewPrivileges=true' "$UNIT"
grep -q -- '--listen 127.0.0.1:8080' "$UNIT"
grep -q -- '--operator-listen 127.0.0.1:8081' "$UNIT"
grep -qx 'Restart=on-failure' "$UNIT"
grep -Eq '^TimeoutStopSec=[1-9][0-9]*s$' "$UNIT"
grep -q 'fern/opencode:SOURCE_COMMIT' "$CONFIG"
! grep -Eq 'fern/opencode:(dev|latest)' "$CONFIG"
grep -qx '  listen: 127.0.0.1:8080' "$CONFIG"
grep -qx '  operatorListen: 127.0.0.1:8081' "$CONFIG"
grep -Eq '^  remoteOrigin: https://[a-z0-9.-]+\.ts\.net(:[0-9]+)?$' "$CONFIG"
! grep -Eq '^  remoteOrigin: http://' "$CONFIG"
grep -q 'REQUIRED: replace' "$CONFIG"
grep -q 'Never expose this with Tailscale Serve' "$CONFIG"

GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 go build -o "$TEMP/fern" "$ROOT/cmd/fern"
strings "$TEMP/fern" >"$TEMP/fern.strings"
grep -q 'Create, restore, and roll back verified offline host backups.' "$TEMP/fern.strings"
grep -q 'fern-host-backup-v1' "$TEMP/fern.strings"
install -m 0640 "$CONFIG" "$TEMP/fern.yaml"
install -m 0640 "$ENV_FILE" "$TEMP/fern.env"
test "$(stat -c %a "$TEMP/fern.yaml" 2>/dev/null || stat -f %Lp "$TEMP/fern.yaml")" = 640
test "$(stat -c %a "$TEMP/fern.env" 2>/dev/null || stat -f %Lp "$TEMP/fern.env")" = 640

if command -v systemd-analyze >/dev/null 2>&1; then
  sed \
    -e "s|^User=fern$|User=$(id -un)|" \
    -e "s|^Group=fern$|Group=$(id -gn)|" \
    -e '/^SupplementaryGroups=docker$/d' \
    -e "s|^WorkingDirectory=.*$|WorkingDirectory=$TEMP|" \
    -e "s|^Environment=HOME=.*$|Environment=HOME=$TEMP|" \
    -e "s|^EnvironmentFile=.*$|EnvironmentFile=$TEMP/fern.env|" \
    -e "s|^ExecStart=.*$|ExecStart=$TEMP/fern up --config $TEMP/fern.yaml --listen 127.0.0.1:8080 --operator-listen 127.0.0.1:8081|" \
    "$UNIT" >"$TEMP/fern.service"
  systemd-analyze verify "$TEMP/fern.service"
else
  printf 'systemd-analyze unavailable; static unit assertions passed\n'
fi

printf 'Fern deployment static checks passed\n'
