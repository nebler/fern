#!/usr/bin/env bash
# Stores Claude's login in the lab, for the boxes and T3's server: the token
# that `claude setup-token` prints (your subscription; not an API key). The
# token is read without echoing and reaches kubectl on stdin, so it is never
# in a command line, a file or the scrollback. Run it again to replace it.
# Needs lab/up.sh.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
if [ -t 0 ]; then
  read -rsp "Paste the token from 'claude setup-token' (it won't show): " token
  echo
else
  read -r token
fi
case "$token" in
  sk-ant-oat*) ;;
  *) echo "that isn't a token from 'claude setup-token' (they start with sk-ant-oat)" >&2; exit 1 ;;
esac
# Boxes read the boxes' namespace's copy, T3 its own.
for ns in fern-boxes fern-system; do
  printf %s "$token" \
    | k -n "$ns" create secret generic claude-login --from-file=token=/dev/stdin --dry-run=client -o yaml \
    | k apply -f - >/dev/null
  k -n "$ns" label secret claude-login "$LABEL" --overwrite >/dev/null
done
unset token
echo "stored the login in fern-boxes and fern-system"
# A pod reads the Secret when it starts.
if k -n fern-system get deploy/t3 >/dev/null 2>&1; then
  k -n fern-system rollout restart deploy/t3 >/dev/null
  k -n fern-system rollout status deploy/t3 --timeout=180s >/dev/null
  echo "restarted T3's server"
fi
awake=$(fern ls | awk 'NR > 1 && $2 == "running" { print $1 }')
if [ -n "$awake" ]; then
  echo "these boxes are awake and still without the login until they sleep: $awake"
fi
