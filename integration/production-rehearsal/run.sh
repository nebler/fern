#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
exec python3 "$ROOT/integration/production-rehearsal/harness.py" "$@"
