#!/usr/bin/env bash
set -Eeuo pipefail

echo "Live standalone GitHub publication is retired. Use the durable task publication API served by fern up, or fern github publish --dry-run for local preflight." >&2
exit 2
