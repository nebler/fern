#!/usr/bin/env bash
# Stores Claude's login on the node, for the boxes and T3's server: the token
# that `claude setup-token` prints (your subscription; not an API key), in
# /etc/fern/claude.env, which only root reads. The token is read without
# echoing and reaches the node on stdin, so it is never in a command line or
# the scrollback. Run it again to replace it. Needs lab/up.sh.
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
# The daemon reads it at every wake; T3 when it starts.
printf 'CLAUDE_CODE_OAUTH_TOKEN=%s\n' "$token" |
  on_node sh -c 'umask 077 && mkdir -p /etc/fern && cat >/etc/fern/claude.env.new && mv /etc/fern/claude.env.new /etc/fern/claude.env'
unset token
echo "stored the login in /etc/fern/claude.env"
if t3_installed; then
  on_node systemctl restart fern-t3 </dev/null
  wait_t3
  echo "restarted T3's server"
fi
awake=$(fern ls | awk 'NR > 1 && $2 == "running" { print $1 }')
if [ -n "$awake" ]; then
  echo "these boxes are awake and still without the login until they sleep: $awake"
fi
