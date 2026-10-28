#!/usr/bin/env bash
# From nothing to a working Fern with T3, in one command; run it again any
# time, as every step skips what is done:
#   lab/setup.sh                 the local lab (a systemd node in Docker on this Mac)
#   FERN_HOST=1.2.3.4 lab/setup.sh   a VPS, as root over SSH (lab/vps.sh)
# It starts or updates the node with T3, asks once for Claude's login
# (`claude setup-token`, your subscription), runs the quick checks and
# prints the pairing link for the T3 app: Settings → Connections.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
lab_lock

need() { command -v "$1" >/dev/null || { echo "setup: needs $1: $2" >&2; exit 1; }; }
[ "${BASH_VERSINFO[0]}" -ge 5 ] || { echo "setup: needs bash 5: brew install bash, first in PATH" >&2; exit 1; }
need docker "Docker Desktop, running"
need node "Node 22 or newer, for lab/t3-run.sh"
docker info >/dev/null 2>&1 || { echo "setup: Docker isn't running" >&2; exit 1; }

if [ -n "$FERN_HOST" ]; then
  echo "== the VPS $FERN_HOST"
  lab/vps.sh "$FERN_HOST"
  port=30774
elif lab_up; then
  echo "== the lab is up; updating its images"
  lab/build.sh t3
  port=30773
else
  echo "== starting the lab"
  lab/up.sh
  lab/build.sh t3
  port=30773
fi

if ! on_node test -s /etc/fern/claude.env </dev/null; then
  echo "== Claude's login: run 'claude setup-token' in another terminal and paste its token"
  lab/claude-login.sh
fi

echo "== quick checks"
lab/check.sh | grep -E '^(FAIL|check:)'

echo "== pairing"
T3_PORT=$port lab/t3-pair.sh
if [ -n "$FERN_HOST" ]; then
  echo "T3 on the VPS is reached through an SSH tunnel; keep this running while you use it:"
  echo "  ssh -N -L 127.0.0.1:$port:127.0.0.1:30773 root@$FERN_HOST"
fi
cat <<EOF
Next:
  lab/dev.sh              after a change: unit tests, build, quick checks (about a minute)
  lab/perf.sh --fast      a quick perf check (about 1.5 minutes, big regressions only)
  lab/perf.sh             the perf gate against lab/perf/$([ -n "$FERN_HOST" ] && echo vps || echo lab).tsv
  lab/t3-run.sh toy 5     the T3 demo, five times, on Haiku
EOF
