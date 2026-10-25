#!/usr/bin/env bash
# Readies T3's server in the lab for the T3 app on this Mac: adds the sample
# repo (Postgres with three users) as a project at repos/sample, once, and
# prints a pairing link for 127.0.0.1:30773 that works for 15 minutes. In the
# app: Settings → Connections, add the link. Needs lab/up.sh and
# lab/build.sh t3. For a VPS (FERN_HOST), T3_PORT names the Mac's end of an
# SSH tunnel to the node's NodePort:
#   ssh -N -L 127.0.0.1:30774:127.0.0.1:30773 root@HOST
set -euo pipefail
source "$(dirname "$0")/lib.sh"
t3() { k -n fern-system exec deploy/t3 -c t3 -- node /opt/t3/apps/server/dist/bin.mjs "$@" </dev/null; }
R=/var/lib/fern/repos/sample
if ! in_t3 "test -d $R/.git" </dev/null; then
  tar_dir lab/sample-repo | in_t3 "
    set -e
    mkdir -p $R && tar -x -C $R -f -
    cd $R && git init -q -b main && git add -A
    git -c user.email=lab@fern -c user.name=lab commit -qm 'Sample repo: Postgres with three users'"
  t3 project add --title sample "$R" >/dev/null
  echo "added the sample project at $R"
fi
# The server names its pod's address; the Mac reaches it through the NodePort.
t3 pair --ttl 15m --label "lab on $(hostname -s)" |
  grep -E '^(Pairing URL|Expires):' |
  sed -E "s#http://[^/]+/#http://127.0.0.1:${T3_PORT:-30773}/#"
