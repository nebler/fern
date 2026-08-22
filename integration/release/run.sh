#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
TEMP=$(mktemp -d "${TMPDIR:-/tmp}/fern-release-test.XXXXXX")
DEFAULT_EVIDENCE="$ROOT/integration/release/artifacts/release-$(date -u +%Y%m%dT%H%M%SZ)-$$"
EVIDENCE=${FERN_RELEASE_ARTIFACTS:-$DEFAULT_EVIDENCE}
trap 'rm -rf "$TEMP"' EXIT

if [[ -e "$EVIDENCE" ]] && [[ -n "$(find "$EVIDENCE" -mindepth 1 -print -quit 2>/dev/null)" ]]; then
  printf 'error: evidence directory is not empty: %s\n' "$EVIDENCE" >&2
  exit 1
fi
mkdir -p "$EVIDENCE"

FIXTURE="$TEMP/repository"
mkdir -p "$FIXTURE/scripts" "$FIXTURE/cmd/fern" "$FIXTURE/deploy/systemd" "$FIXTURE/deploy/release"
cp "$ROOT/scripts/build-release.sh" "$FIXTURE/scripts/"
cp "$ROOT/deploy/systemd/"* "$FIXTURE/deploy/systemd/"
cp "$ROOT/deploy/release/"* "$FIXTURE/deploy/release/"
cat >"$FIXTURE/go.mod" <<'EOF'
module example.invalid/release-fixture

go 1.24.0
EOF
cat >"$FIXTURE/cmd/fern/main.go" <<'EOF'
package main

var version = "dev"
var commit = "unknown"

func main() {
	println(version, commit)
}
EOF
printf 'dist/\n' >"$FIXTURE/.gitignore"

git -C "$FIXTURE" init -q
git -C "$FIXTURE" config user.name 'Fern release test'
git -C "$FIXTURE" config user.email 'release-test@fern.invalid'
git -C "$FIXTURE" add .
GIT_AUTHOR_DATE=2026-01-02T03:04:05Z GIT_COMMITTER_DATE=2026-01-02T03:04:05Z \
  git -C "$FIXTURE" commit -qm 'release fixture'
FIXTURE_COMMIT=$(git -C "$FIXTURE" rev-parse HEAD)

(
  cd "$FIXTURE"
  GOTOOLCHAIN=local ./scripts/build-release.sh v1.2.3
  shasum -a 256 -c dist/SHA256SUMS >"$TEMP/checksum-verification.txt"
)

python3 - "$FIXTURE" "$FIXTURE_COMMIT" <<'PY'
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
commit = sys.argv[2]
manifest = json.loads((root / "dist/RELEASE-MANIFEST.json").read_text())
assert manifest["schema_version"] == 1
assert manifest["release"]["commit"] == commit
assert manifest["release"]["source_date_epoch"] == 1767323045
assert manifest["release"]["version_source"] == "builder-argument"
assert manifest["integrity"] == {
    "checksum_algorithm": "sha256",
    "signature_status": "not-generated",
}
assert manifest["upgrade_rollback"] == {
    "transaction_manifest": "deploy/release/transaction-manifest.example.json",
    "support_status": "not-implemented",
    "sqlite_backup": "placeholder-only-not-implemented",
    "application_secrets_backup": "placeholder-only-not-implemented",
}
for entry in manifest["artifacts"] + manifest["deployment_files"]:
    actual = hashlib.sha256((root / "dist" / entry["path"]).read_bytes()).hexdigest()
    assert actual == entry["sha256"], entry["path"]

for path in root.glob("deploy/release/*.json"):
    json.loads(path.read_text())
transaction = json.loads((root / "deploy/release/transaction-manifest.example.json").read_text())
assert transaction["compatibility"]["status"] == "not-implemented"
assert transaction["backup"]["status"] == "not-implemented"
assert transaction["backup"]["sqlite"].startswith("PLACEHOLDER:")
assert transaction["backup"]["application_secrets"].startswith("PLACEHOLDER:")
assert transaction["rollback"]["status"] == "not-implemented"
PY

(
  cd "$FIXTURE/dist"
  find . -type f -print | LC_ALL=C sort | while IFS= read -r file; do shasum -a 256 "$file"; done
) >"$TEMP/first-build.sha256"
cp "$FIXTURE/dist/RELEASE-MANIFEST.json" "$TEMP/RELEASE-MANIFEST.json"

SECOND_FIXTURE="$TEMP/repository-second"
git clone -q "$FIXTURE" "$SECOND_FIXTURE"
(
  cd "$SECOND_FIXTURE"
  GOTOOLCHAIN=local ./scripts/build-release.sh v1.2.3
)
(
  cd "$SECOND_FIXTURE/dist"
  find . -type f -print | LC_ALL=C sort | while IFS= read -r file; do shasum -a 256 "$file"; done
) >"$TEMP/second-build.sha256"
diff -u "$TEMP/first-build.sha256" "$TEMP/second-build.sha256" >"$TEMP/reproducibility.diff"

printf '\ncorruption\n' >>"$SECOND_FIXTURE/dist/fern-v1.2.3-linux-amd64"
if (cd "$SECOND_FIXTURE" && shasum -a 256 -c dist/SHA256SUMS) >"$TEMP/tamper-check.txt" 2>&1; then
  printf 'error: checksum verification accepted a modified binary\n' >&2
  exit 1
fi

printf '\n// dirty\n' >>"$FIXTURE/cmd/fern/main.go"
if (cd "$FIXTURE" && GOTOOLCHAIN=local ./scripts/build-release.sh v1.2.4) >"$TEMP/dirty-check.txt" 2>&1; then
  printf 'error: release build accepted a dirty tree\n' >&2
  exit 1
fi
grep -q 'clean working tree' "$TEMP/dirty-check.txt"
git -C "$FIXTURE" restore cmd/fern/main.go
if (cd "$FIXTURE" && GOTOOLCHAIN=local ./scripts/build-release.sh latest) >"$TEMP/version-check.txt" 2>&1; then
  printf 'error: release build accepted a non-semantic version\n' >&2
  exit 1
fi
grep -q 'semantic version' "$TEMP/version-check.txt"

UNIT="$ROOT/deploy/systemd/fern.service"
grep -qx 'User=fern' "$UNIT"
grep -qx 'NoNewPrivileges=true' "$UNIT"
grep -qx 'ProtectHome=true' "$UNIT"
grep -qx 'ProtectKernelTunables=true' "$UNIT"
grep -qx 'ProtectKernelModules=true' "$UNIT"
grep -qx 'ProtectControlGroups=true' "$UNIT"
grep -qx 'RestrictSUIDSGID=true' "$UNIT"
grep -qx 'LockPersonality=true' "$UNIT"
grep -Eq '^ExecStart=.*--listen 127\.0\.0\.1:8080 --operator-listen 127\.0\.0\.1:8081$' "$UNIT"
! grep -Eiq 'Exec(Start|Stop).*\b(docker|tailscale)\b.*\b(rm|reset|funnel)\b' "$UNIT"
grep -Eq '^[[:space:]]*(sudo[[:space:]]+)?tailscale[[:space:]]+serve[[:space:]]+--bg[[:space:]]+http://127\.0\.0\.1:8080' "$ROOT/docs/DEPLOYMENT.md"
! grep -Eq '^[[:space:]]*(sudo[[:space:]]+)?tailscale[[:space:]]+funnel([[:space:]]|$)' "$ROOT/docs/DEPLOYMENT.md"
! grep -REn -- '--listen[[:space:]]+(0\.0\.0\.0|\[?::\]?)(:|[[:space:]])' "$ROOT/deploy"
grep -qx '  listen: 127.0.0.1:8080' "$ROOT/deploy/systemd/fern.yaml.example"
grep -qx '  operatorListen: 127.0.0.1:8081' "$ROOT/deploy/systemd/fern.yaml.example"
grep -Eq '^  remoteOrigin: https://[a-z0-9.-]+\.ts\.net(:[0-9]+)?$' "$ROOT/deploy/systemd/fern.yaml.example"
! grep -Eq '^  remoteOrigin: http://' "$ROOT/deploy/systemd/fern.yaml.example"
grep -q 'REQUIRED: replace' "$ROOT/deploy/systemd/fern.yaml.example"
grep -q 'Never expose this with Tailscale Serve' "$ROOT/deploy/systemd/fern.yaml.example"

cp "$TEMP/RELEASE-MANIFEST.json" "$EVIDENCE/release-manifest.json"
cp "$ROOT/deploy/release/transaction-manifest.example.json" "$EVIDENCE/transaction-manifest.example.json"
cp "$TEMP/checksum-verification.txt" "$EVIDENCE/checksum-verification.txt"
cp "$TEMP/tamper-check.txt" "$EVIDENCE/tamper-rejection.txt"
cat >"$EVIDENCE/static-assertions.txt" <<'EOF'
PASS systemd runs as fern with explicit hardening and distinct remote/operator loopback listeners
PASS systemd contains no Docker/Tailscale destructive lifecycle command
PASS deployment assets contain no wildcard Fern listener
PASS deployment assets identify only the remote loopback listener for Tailscale Serve
PASS deployment configuration requires an exact HTTPS remote origin replacement
PASS deployment runbook contains no executable Tailscale Funnel command
EOF
cat >"$EVIDENCE/summary.json" <<EOF
{
  "schema_version": 1,
  "fixture_commit": "$FIXTURE_COMMIT",
  "checks": {
    "reproducible_build": "passed",
    "manifest_artifact_hashes": "passed",
    "checksum_tamper_rejection": "passed",
    "dirty_tree_rejection": "passed",
    "semantic_version_rejection": "passed",
    "static_systemd_tailscale_safety": "passed"
  },
  "not_run": {
    "artifact_signing": "not generated; this bundle provides checksums, not authenticity",
    "ubuntu_systemd_host": "requires an explicit target host",
    "tailscale_mutation": "intentionally excluded from this static harness",
    "docker_mutation": "intentionally excluded from this static harness",
    "sqlite_backup": "not implemented; placeholder only",
    "application_secrets_backup": "not implemented; placeholder only",
    "upgrade_or_rollback": "manifest shape only; execution is not implemented"
  }
}
EOF
(
  cd "$EVIDENCE"
  find . -type f ! -name SHA256SUMS -print | LC_ALL=C sort | sed 's#^./##' | \
    while IFS= read -r file; do shasum -a 256 "$file"; done >SHA256SUMS
  shasum -a 256 -c SHA256SUMS >/dev/null
)

printf 'Fern release checks passed; evidence: %s\n' "$EVIDENCE"
