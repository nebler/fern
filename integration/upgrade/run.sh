#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
TEMP=$(mktemp -d "${TMPDIR:-/tmp}/fern-schema-test.XXXXXX")
CURRENT="$TEMP/current"
BACKUP="$TEMP/backup"
trap 'rm -rf "$TEMP"' EXIT

mkdir -m 0700 "$CURRENT" "$BACKUP"

(cd "$ROOT" && go test ./internal/compatibility -count=1)
(cd "$ROOT" && go run ./integration/upgrade --database "$CURRENT/task-store.sqlite")

cp "$CURRENT/task-store.sqlite" "$BACKUP/task-store.sqlite"
chmod 0600 "$BACKUP/task-store.sqlite"

# Reopening the current schema must not require a second migration.
(cd "$ROOT" && go run ./integration/upgrade --database "$CURRENT/task-store.sqlite")

# Exercise offline byte restoration without pretending an older schema exists.
printf 'not a sqlite database' >"$CURRENT/task-store.sqlite"
cp "$BACKUP/task-store.sqlite" "$CURRENT/task-store.sqlite"
chmod 0600 "$CURRENT/task-store.sqlite"
(cd "$ROOT" && go run ./integration/upgrade --database "$CURRENT/task-store.sqlite")

printf 'Fern schema-1 initialization, reopen, and offline rollback checks passed\n'
