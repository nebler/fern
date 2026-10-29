#!/usr/bin/env bash
# Runs sqlc (config in sqlc.yaml) in its container: lab/sqlc.sh generate
# after changing internal/store/index/*.sql, lab/sqlc.sh diff to check the
# generated code is up to date.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
exec docker run --rm --label "$LABEL" -v "$PWD:/src" -w /src sqlc/sqlc:1.31.1 "$@"
