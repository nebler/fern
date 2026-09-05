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
grep -qx '  listen: 127.0.0.1:8080' "$CONFIG"
grep -qx '  operatorListen: 127.0.0.1:8081' "$CONFIG"
grep -Eq '^  remoteOrigin: https://[a-z0-9.-]+\.ts\.net(:[0-9]+)?$' "$CONFIG"
! grep -Eq '^  remoteOrigin: http://' "$CONFIG"

python3 - "$CONFIG" <<'PY'
import ipaddress
import pathlib
import re
import sys
import urllib.parse

config = pathlib.Path(sys.argv[1]).read_text()

def exact(pattern, source, message):
    match = re.search(pattern, source, re.MULTILINE)
    assert match, message
    return match.group(1) if match.lastindex else match.group(0)

exact(r'^  backgroundImage: ghcr\.io/owner/fern/opencode-background-source@sha256:REPLACE_WITH_64_LOWERCASE_HEX_DIGITS$', config,
      'deployment example lacks an immutable Background Run source image')
exact(r'^  backgroundImageID: sha256:REPLACE_WITH_64_LOWERCASE_HEX_DIGITS$', config,
      'deployment example lacks an explicit immutable Background Run image ID')
background_listen = exact(r'^    listen: (\S+)$', config, 'Background Run listener is absent')
background_origin = urllib.parse.urlsplit(exact(r'^    origin: (\S+)$', config, 'Background Run origin is absent'))
proxy_listen = exact(r'^  listen: (\S+)$', config, 'remote listener is absent')
operator_listen = exact(r'^  operatorListen: (\S+)$', config, 'operator listener is absent')
remote_origin = urllib.parse.urlsplit(exact(r'^  remoteOrigin: (\S+)$', config, 'remote origin is absent'))

host, port = background_listen.rsplit(':', 1)
assert ipaddress.ip_address(host).is_loopback and host == '127.0.0.1', 'Background Run listener must use exact numeric loopback'
assert port.isdigit() and 1 <= int(port) <= 65535, 'Background Run listener port must be explicit'
assert background_origin.scheme == 'https' and background_origin.hostname == remote_origin.hostname, \
    'Background Run and root origins must use the same private hostname'
assert remote_origin.scheme == 'https' and remote_origin.hostname.endswith('.ts.net') and remote_origin.port is None, \
    'root origin must be canonical private HTTPS on implicit 443'
assert background_origin.port == int(port) and background_origin.port != 443, \
    'Background Run origin must use the listener\'s explicit non-443 port'
assert background_listen not in (proxy_listen, operator_listen), 'Background Run listener must be distinct'

PY

GOOS=linux GOARCH=amd64 go build -o "$TEMP/fern" "$ROOT/cmd/fern"
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
