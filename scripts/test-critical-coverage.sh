#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUN_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/fern-critical-coverage.XXXXXX")
trap 'rm -rf "$RUN_ROOT"' EXIT

PROFILE="$RUN_ROOT/coverage.out"
FUNCTIONS="$RUN_ROOT/functions.txt"
MODULE=github.com/nebler/fern
PACKAGES=(
  internal/taskenvdocker
  internal/taskartifact
  internal/runapi
  internal/resultapi
  internal/taskverification
  internal/taskpublicationcoord
  internal/taskstore
)

cd "$ROOT"
go test -count=1 -timeout=3m -covermode=atomic -coverprofile="$PROFILE" "${PACKAGES[@]/#/.\/}"
go tool cover -func="$PROFILE" >"$FUNCTIONS"

check_package() {
  local package=$1 floor=$2 coverage
  coverage=$(awk -v prefix="$MODULE/$package/" '
    index($1, prefix) == 1 { total += $2; if ($3 > 0) covered += $2 }
    END { if (total == 0) exit 2; printf "%.1f", covered * 100 / total }
  ' "$PROFILE")
  awk -v got="$coverage" -v floor="$floor" 'BEGIN { exit !(got + 0 >= floor + 0) }' || {
    printf 'critical coverage: %s is %s%%, below %s%%\n' "$package" "$coverage" "$floor" >&2
    exit 1
  }
  printf 'critical coverage: %-38s %5s%% (floor %s%%)\n' "$package" "$coverage" "$floor"
}

check_function() {
  local file=$1 symbol=$2 floor=$3 coverage
  coverage=$(awk -v suffix="/$file" -v symbol="$symbol" '
    index($1, suffix ":") > 0 && $2 == symbol { value=$3; sub(/%$/, "", value); print value }
  ' "$FUNCTIONS")
  if [[ -z "$coverage" ]]; then
    printf 'critical coverage: missing function %s.%s\n' "$file" "$symbol" >&2
    exit 1
  fi
  awk -v got="$coverage" -v floor="$floor" 'BEGIN { exit !(got + 0 >= floor + 0) }' || {
    printf 'critical coverage: %s.%s is %s%%, below %s%%\n' "$file" "$symbol" "$coverage" "$floor" >&2
    exit 1
  }
  printf 'critical function: %-39s %5s%% (floor %s%%)\n' "$file.$symbol" "$coverage" "$floor"
}

check_package internal/taskenvdocker 72
check_package internal/taskartifact 70
check_package internal/runapi 68
check_package internal/resultapi 74
check_package internal/taskverification 72
check_package internal/taskpublicationcoord 80
check_package internal/taskstore 60

check_function internal/taskverification/coordinator.go RunOnce 80
check_function internal/taskpublicationcoord/coordinator.go RunOnce 85
