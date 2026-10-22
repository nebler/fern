#!/usr/bin/env bash
# Readies T3's server in the lab for the T3 app on this Mac: adds the sample
# repo (Postgres with three users) as a project at repos/sample, once, and
# prints a pairing link for 127.0.0.1:30773 that works for 15 minutes. In the
# app: Settings → Connections, add the link. Needs lab/up.sh, lab/build-t3.sh
# and deploy/t3.yaml (lab/blockers.sh t3 applies it).
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."
t3() { k -n fern-system exec -i deploy/t3 -c t3 -- node /opt/t3/apps/server/dist/bin.mjs "$@"; }
R=/var/lib/fern/repos/sample
if ! k -n fern-system exec deploy/t3 -c t3 -- test -d "$R/.git" 2>/dev/null; then
  COPYFILE_DISABLE=1 tar -C lab/sample-repo -cf - . | k -n fern-system exec -i deploy/t3 -c t3 -- sh -c "
    set -e
    mkdir -p $R && tar --warning=no-unknown-keyword -x -C $R -f -
    cd $R && git init -q -b main && git add -A
    git -c user.email=lab@fern -c user.name=lab commit -qm 'Sample repo: Postgres with three users'"
  t3 project add --title sample "$R" </dev/null >/dev/null
  echo "added the sample project at $R"
fi
# The server names its pod's address; the Mac reaches it through the NodePort.
t3 pair --ttl 15m --label "lab on $(hostname -s)" </dev/null \
  | grep -E '^(Pairing URL|Expires):' \
  | sed -E 's#http://[^/]+/#http://127.0.0.1:30773/#'
