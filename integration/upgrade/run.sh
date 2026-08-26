#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
BASELINE="$ROOT/internal/compatibility/testdata/baseline-v1"
COMPATIBILITY="$ROOT/deploy/release/compatibility-manifest.json"
TEMP=$(mktemp -d "${TMPDIR:-/tmp}/fern-upgrade-test.XXXXXX")
WORK="$TEMP/current"
PRE_UPGRADE="$TEMP/pre-upgrade"
trap 'rm -rf "$TEMP"' EXIT

verify_baseline() {
  python3 - "$1" "$COMPATIBILITY" <<'PY'
import hashlib
import json
import pathlib
import sqlite3
import sys

root = pathlib.Path(sys.argv[1])
compatibility = json.loads(pathlib.Path(sys.argv[2]).read_text())
metadata_path = root / "metadata.json"
metadata_bytes = metadata_path.read_bytes()
assert hashlib.sha256(metadata_bytes).hexdigest() == compatibility["verification"]["fixture_metadata_sha256"]
metadata = json.loads(metadata_bytes)
assert metadata["baseline"] == compatibility["first_supported_baseline"]["id"]
assert metadata["provenance"]["historical_release"] is False
assert metadata["provenance"]["historical_tag"] is None
assert metadata["schemas"]["task_store"]["fixture_version"] == 4
assert metadata["schemas"]["task_store"]["upgrade_target_version"] == compatibility["current_release_schemas"]["task_store"]
for relative, expected in metadata["files"].items():
    actual = hashlib.sha256((root / relative).read_bytes()).hexdigest()
    assert actual == expected, relative

database = sqlite3.connect(f"file:{root / 'task-store.sqlite'}?mode=ro&immutable=1", uri=True)
assert database.execute("PRAGMA user_version").fetchone()[0] == 4
assert database.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
assert database.execute("PRAGMA foreign_key_check").fetchall() == []
ledger = database.execute("SELECT version,name,checksum FROM schema_migrations ORDER BY version").fetchall()
expected_ledger = [(entry["version"], entry["name"], entry["sha256"]) for entry in metadata["schemas"]["task_store"]["migration_ledger"][:4]]
assert ledger == expected_ledger
database.close()
PY
}

verify_baseline "$BASELINE"
(cd "$ROOT" && go test ./internal/compatibility -run '^TestBaselineV1UpgradesWithoutSemanticLoss$' -count=1)

mkdir -m 0700 "$WORK" "$PRE_UPGRADE"
cp -R "$BASELINE/." "$WORK/"
find "$WORK" -type d -exec chmod 0700 {} +
find "$WORK" -type f -exec chmod 0600 {} +
cp -R "$WORK/." "$PRE_UPGRADE/"
find "$PRE_UPGRADE" -type d -exec chmod 0700 {} +
find "$PRE_UPGRADE" -type f -exec chmod 0600 {} +
verify_baseline "$PRE_UPGRADE"

(cd "$ROOT" && go run ./integration/upgrade --database "$WORK/task-store.sqlite")
if cmp -s "$WORK/task-store.sqlite" "$PRE_UPGRADE/task-store.sqlite"; then
  printf 'error: migration did not change the copied task database\n' >&2
  exit 1
fi

# Simulate failed activation: discard the migrated generation and restore the
# verified offline copy. Older code must never receive post-migration bytes.
rm -rf "$WORK"
mkdir -m 0700 "$WORK"
cp -R "$PRE_UPGRADE/." "$WORK/"
find "$WORK" -type d -exec chmod 0700 {} +
find "$WORK" -type f -exec chmod 0600 {} +
verify_baseline "$WORK"

# A restored generation remains a valid source for a later retry.
(cd "$ROOT" && go run ./integration/upgrade --database "$WORK/task-store.sqlite")
verify_baseline "$PRE_UPGRADE"

printf 'Fern baseline-v1 upgrade, pre-upgrade backup, and rollback checks passed\n'
