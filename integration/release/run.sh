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
cp "$ROOT/scripts/build-release.sh" "$ROOT/scripts/create-release-bundle.py" "$FIXTURE/scripts/"
cp "$ROOT/deploy/systemd/"* "$FIXTURE/deploy/systemd/"
cp "$ROOT/deploy/release/"* "$FIXTURE/deploy/release/"
cat >"$FIXTURE/go.mod" <<'EOF'
module example.invalid/release-fixture

go 1.27.0
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
  ./scripts/build-release.sh v1.2.3
  (cd dist && shasum -a 256 -c SHA256SUMS) >"$TEMP/checksum-verification.txt"
)

python3 - "$FIXTURE" "$FIXTURE_COMMIT" <<'PY'
import hashlib
import json
import pathlib
import sys
import tarfile

root = pathlib.Path(sys.argv[1])
commit = sys.argv[2]
manifest = json.loads((root / "dist/RELEASE-MANIFEST.json").read_text())
assert manifest["schema_version"] == 2
assert manifest["release"]["commit"] == commit
assert manifest["release"]["source_date_epoch"] == 1767323045
assert manifest["release"]["version_source"] == "builder-argument"
assert manifest["release"]["verified_tag"] is None
assert manifest["integrity"] == {
    "checksum_algorithm": "sha256",
    "binary_signature_status": "not-generated",
    "asset_provenance_status": "not-generated-local",
}
assert manifest["image"] == {
    "repository": "fern/opencode-background-source",
    "publication_status": "not-published-local",
    "digest": None,
    "reference": None,
    "sbom": {"status": "not-generated-local", "format": None, "asset": None, "sha256": None, "attestation_subject": None},
    "signature": {"status": "not-generated-local", "subject": None, "certificate_identity": None, "oidc_issuer": None},
    "provenance": {"status": "not-generated-local", "subject": None, "predicate_type": None, "attestation_url": None},
}
assert manifest["upgrade_rollback"] == {
    "compatibility_manifest": "deploy/release/compatibility-manifest.json",
    "first_supported_baseline": None,
    "upgrade_harness": "integration/upgrade/run.sh",
    "backup_command": "fern backup",
    "backup_manifest_schema": "deploy/release/backup-manifest.schema.json",
    "support_status": "pre-release-schema-reset",
    "activation_model": "state-directory-swap-with-previous",
    "credential_policy": "age-encrypted-archive",
    "volume_export_mode": "no-runtime-volumes",
}
bundle = root / "dist" / manifest["distribution"]["bundle_asset"]
extract = root / "extracted"
with tarfile.open(bundle, "r:gz") as archive:
    archive.extractall(extract, filter="data")
bundle_root = extract / manifest["distribution"]["bundle_root"]
assert json.loads((bundle_root / "RELEASE-MANIFEST.json").read_text()) == manifest
for entry in manifest["artifacts"] + manifest["deployment_files"]:
    actual = hashlib.sha256((bundle_root / entry["path"]).read_bytes()).hexdigest()
    assert actual == entry["sha256"], entry["path"]
checksummed = {
    line.split("  ", 1)[1]
    for line in (root / "dist/SHA256SUMS").read_text().splitlines()
}
downloaded = {path.name for path in (root / "dist").iterdir() if path.name != "SHA256SUMS"}
assert checksummed == downloaded
assert all(path.is_file() for path in (root / "dist").iterdir())

for path in root.glob("deploy/release/*.json"):
    json.loads(path.read_text())
compatibility = json.loads((root / "deploy/release/compatibility-manifest.json").read_text())
compatibility_schema = json.loads((root / "deploy/release/compatibility-manifest.schema.json").read_text())
assert compatibility["schema_version"] == 1
assert compatibility["first_supported_baseline"] is None
assert compatibility["current_release_schemas"]["task_store"] == 8
assert compatibility["current_release_schemas"]["control_state"] == 2
current_schema = compatibility_schema["properties"]["current_release_schemas"]
assert current_schema["additionalProperties"] is False
assert current_schema["properties"]["task_store"]["const"] == compatibility["current_release_schemas"]["task_store"]
backup_schema = json.loads((root / "deploy/release/backup-manifest.schema.json").read_text())
assert backup_schema["properties"]["format"]["const"] == "fern-backup-v2"
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
  ./scripts/build-release.sh v1.2.3
)
(
  cd "$SECOND_FIXTURE/dist"
  find . -type f -print | LC_ALL=C sort | while IFS= read -r file; do shasum -a 256 "$file"; done
) >"$TEMP/second-build.sha256"
diff -u "$TEMP/first-build.sha256" "$TEMP/second-build.sha256" >"$TEMP/reproducibility.diff"

PUBLISHED_FIXTURE="$TEMP/repository-published"
git clone -q "$FIXTURE" "$PUBLISHED_FIXTURE"
printf '{"spdxVersion":"SPDX-2.3","packages":[{"name":"fern-opencode-background-source"}]}\n' >"$TEMP/image.spdx.json"
(
  cd "$PUBLISHED_FIXTURE"
  FERN_VERIFIED_TAG=v1.2.3 \
    FERN_IMAGE_REPOSITORY=ghcr.io/example/fern/opencode-background-source \
    FERN_IMAGE_DIGEST="sha256:$(printf 'a%.0s' {1..64})" \
    FERN_IMAGE_SBOM_PATH="$TEMP/image.spdx.json" \
    FERN_IMAGE_PROVENANCE_URL=https://github.com/example/fern/attestations/123 \
    FERN_IMAGE_CERTIFICATE_IDENTITY=https://github.com/example/fern/.github/workflows/release.yml@refs/tags/v1.2.3 \
    FERN_IMAGE_OIDC_ISSUER=https://token.actions.githubusercontent.com \
    ./scripts/build-release.sh v1.2.3
  (cd dist && shasum -a 256 -c SHA256SUMS)
)
python3 - "$PUBLISHED_FIXTURE/dist/RELEASE-MANIFEST.json" <<'PY'
import json, pathlib, sys
manifest = json.loads(pathlib.Path(sys.argv[1]).read_text())
digest = "sha256:" + "a" * 64
subject = "ghcr.io/example/fern/opencode-background-source@" + digest
assert manifest["release"]["version_source"] == "verified-annotated-tag"
assert manifest["release"]["verified_tag"] == "v1.2.3"
assert manifest["image"]["digest"] == digest
assert manifest["image"]["reference"] == subject
assert manifest["image"]["sbom"]["status"] == "generated-and-attested"
assert manifest["image"]["sbom"]["attestation_subject"] == subject
assert manifest["image"]["signature"]["status"] == "verified-keyless"
assert manifest["image"]["signature"]["subject"] == subject
assert manifest["image"]["provenance"]["status"] == "verified-github-attestation"
assert manifest["image"]["provenance"]["subject"] == subject
PY

printf '\ncorruption\n' >>"$SECOND_FIXTURE/dist/fern-v1.2.3-linux-amd64"
if (cd "$SECOND_FIXTURE/dist" && shasum -a 256 -c SHA256SUMS) >"$TEMP/tamper-check.txt" 2>&1; then
  printf 'error: checksum verification accepted a modified binary\n' >&2
  exit 1
fi

printf '\n// dirty\n' >>"$FIXTURE/cmd/fern/main.go"
if (cd "$FIXTURE" && ./scripts/build-release.sh v1.2.4) >"$TEMP/dirty-check.txt" 2>&1; then
  printf 'error: release build accepted a dirty tree\n' >&2
  exit 1
fi
grep -q 'clean working tree' "$TEMP/dirty-check.txt"
git -C "$FIXTURE" restore cmd/fern/main.go
if (cd "$FIXTURE" && ./scripts/build-release.sh latest) >"$TEMP/version-check.txt" 2>&1; then
  printf 'error: release build accepted a non-semantic version\n' >&2
  exit 1
fi
grep -q 'semantic version' "$TEMP/version-check.txt"

FERN="$TEMP/fern"
(cd "$ROOT" && go build -o "$FERN" ./cmd/fern)
RECIPIENT=$(cd "$ROOT" && go run ./integration/release/agekey "$TEMP/identity.txt")
(cd "$ROOT" && go run ./integration/release/agekey "$TEMP/other-identity.txt") >/dev/null

HOST="$TEMP/host"
SOURCE="$HOST/source"
STATE="$SOURCE/home/.fern"
mkdir -p "$SOURCE/etc" "$HOST/repository" "$STATE/control" "$STATE/github-app" \
  "$STATE/tasks/demo-background/artifact-cas/sha256:abc" "$STATE/tasks/demo-background/artifact-work" \
  "$STATE/tasks/demo-background/runtime/background-runs/clone" "$STATE/locks"
sed -e "s|/srv/fern/repository|$HOST/repository|" \
  -e "s|sha256:REPLACE_WITH_QUALIFIED_LOCAL_IMAGE_ID|sha256:$(printf 'b%.0s' {1..64})|" \
  "$ROOT/fern.example.yaml" >"$SOURCE/etc/fern.yaml"
printf 'FERN_CONTROL_PASSWORD=secret-control-password-0123456789\n' >"$SOURCE/etc/fern.env"
chmod 0600 "$SOURCE/etc/fern.yaml" "$SOURCE/etc/fern.env"
printf 'devices-a\n' >"$STATE/control/devices.json"
printf '{"client_secret":"secret-app","private_key":"secret-private-key"}\n' >"$STATE/github-app/app-credentials.json"
printf 'artifact-a\n' >"$STATE/tasks/demo-background/artifact-cas/sha256:abc/manifest.json"
printf 'scratch\n' >"$STATE/tasks/demo-background/artifact-work/scratch"
printf 'secret-host-key-0123456789abcdef' >"$STATE/tasks/demo-background/runtime/background-runs/host.key"
printf 'clone\n' >"$STATE/tasks/demo-background/runtime/background-runs/clone/file"
python3 - "$STATE/tasks/demo.db" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("PRAGMA journal_mode=WAL")
db.execute("CREATE TABLE runs (id TEXT)")
db.execute("INSERT INTO runs VALUES ('run-a')")
db.commit()
PY

fern_at() {
  local root=$1
  shift
  local command=$1 subcommand=$2
  shift 2
  HOME="$root/home" "$FERN" "$command" "$subcommand" --config "$root/etc/fern.yaml" \
    --env-file "$root/etc/fern.env" --state-dir "$root/home/.fern" "$@"
}
run_sql() {
  python3 - "$1" <<'PY'
import sqlite3, sys
print(sqlite3.connect(sys.argv[1]).execute("SELECT id FROM runs").fetchone()[0])
PY
}

fern_at "$SOURCE" backup create --recipient "$RECIPIENT" --output "$HOST/backup-a"
test "$(stat -c %a "$HOST/backup-a" 2>/dev/null || stat -f %Lp "$HOST/backup-a")" = 600
! grep -a -q 'secret-app\|secret-private-key\|secret-control-password\|secret-host-key\|devices-a' "$HOST/backup-a"
if fern_at "$SOURCE" backup create --recipient "$RECIPIENT" --output "$HOST/backup-a" >"$TEMP/overwrite-rejection.txt" 2>&1; then
  printf 'error: backup replaced an existing output\n' >&2
  exit 1
fi
grep -q 'already exists' "$TEMP/overwrite-rejection.txt"

reject_restore() {
  local input=$1 identity=$2 evidence=$3 expected=$4
  local target="$HOST/rejected-$evidence"
  mkdir -p "$target/etc"
  if fern_at "$target" backup restore --identity "$identity" --input "$input" >"$TEMP/$evidence.txt" 2>&1; then
    printf 'error: restore accepted %s\n' "$evidence" >&2
    exit 1
  fi
  grep -q "$expected" "$TEMP/$evidence.txt"
  test ! -e "$target/home/.fern" && test ! -e "$target/etc/fern.yaml"
}
cp "$HOST/backup-a" "$HOST/tampered"
printf 'X' | dd of="$HOST/tampered" bs=1 seek=400 conv=notrunc 2>/dev/null
reject_restore "$HOST/tampered" "$TEMP/identity.txt" backup-tamper-rejection 'backup'
head -c "$(($(wc -c <"$HOST/backup-a") - 32))" "$HOST/backup-a" >"$HOST/truncated"
reject_restore "$HOST/truncated" "$TEMP/identity.txt" truncation-rejection 'read backup'
reject_restore "$HOST/backup-a" "$TEMP/other-identity.txt" wrong-identity-rejection 'decrypt backup'

TARGET="$HOST/target"
mkdir -p "$TARGET/etc"
fern_at "$TARGET" backup restore --identity "$TEMP/identity.txt" --input "$HOST/backup-a"
TARGET_STATE="$TARGET/home/.fern"
grep -qx 'devices-a' "$TARGET_STATE/control/devices.json"
grep -q 'secret-private-key' "$TARGET_STATE/github-app/app-credentials.json"
grep -qx 'artifact-a' "$TARGET_STATE/tasks/demo-background/artifact-cas/sha256:abc/manifest.json"
cmp "$STATE/tasks/demo-background/runtime/background-runs/host.key" \
  "$TARGET_STATE/tasks/demo-background/runtime/background-runs/host.key"
cmp "$SOURCE/etc/fern.yaml" "$TARGET/etc/fern.yaml"
cmp "$SOURCE/etc/fern.env" "$TARGET/etc/fern.env"
test "$(run_sql "$TARGET_STATE/tasks/demo.db")" = run-a
test ! -e "$TARGET_STATE/tasks/demo-background/artifact-work"
test ! -e "$TARGET_STATE/tasks/demo-background/runtime/background-runs/clone"
test ! -e "$TARGET_STATE.previous"

printf 'devices-b\n' >"$STATE/control/devices.json"
if fern_at "$SOURCE" backup restore --identity "$TEMP/identity.txt" --input "$HOST/backup-a" >"$TEMP/live-state-rejection.txt" 2>&1; then
  printf 'error: restore replaced live state without --replace\n' >&2
  exit 1
fi
grep -q -- '--replace' "$TEMP/live-state-rejection.txt"
fern_at "$SOURCE" backup restore --identity "$TEMP/identity.txt" --input "$HOST/backup-a" --replace
grep -qx 'devices-a' "$STATE/control/devices.json"
grep -qx 'devices-b' "$STATE.previous/control/devices.json"
test -e "$SOURCE/etc/fern.yaml.previous"
fern_at "$SOURCE" backup rollback
grep -qx 'devices-b' "$STATE/control/devices.json"
grep -qx 'devices-a' "$STATE.previous/control/devices.json"
test "$(run_sql "$STATE/tasks/demo.db")" = run-a

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
! grep -REn -- '--listen[[:space:]]+(0\.0\.0\.0|\[?::\]?)(:|[[:space:]])' "$ROOT/deploy"
grep -qx '  listen: 127.0.0.1:8080' "$ROOT/deploy/systemd/fern.yaml.example"
grep -qx '  operatorListen: 127.0.0.1:8081' "$ROOT/deploy/systemd/fern.yaml.example"
grep -Eq '^  remoteOrigin: https://[a-z0-9.-]+\.ts\.net(:[0-9]+)?$' "$ROOT/deploy/systemd/fern.yaml.example"
! grep -Eq '^  remoteOrigin: http://' "$ROOT/deploy/systemd/fern.yaml.example"

cp "$TEMP/RELEASE-MANIFEST.json" "$EVIDENCE/release-manifest.json"
cp "$TEMP/checksum-verification.txt" "$EVIDENCE/checksum-verification.txt"
cp "$TEMP/tamper-check.txt" "$EVIDENCE/tamper-rejection.txt"
for name in backup-tamper-rejection truncation-rejection wrong-identity-rejection live-state-rejection overwrite-rejection; do
  cp "$TEMP/$name.txt" "$EVIDENCE/$name.txt"
done
cat >"$EVIDENCE/static-assertions.txt" <<'EOF'
PASS systemd runs as fern with explicit hardening and distinct remote/operator loopback listeners
PASS systemd contains no Docker/Tailscale destructive lifecycle command
PASS deployment assets contain no wildcard Fern listener
PASS deployment configuration requires an exact HTTPS remote origin replacement
PASS fern backup create writes one age-encrypted archive with no plaintext secrets and never overwrites
PASS fern backup restore rejects tampering, truncation, and the wrong identity without touching the target
PASS fern backup restore requires --replace over live state, keeps it as .previous, and rollback swaps it back
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
    "static_systemd_tailscale_safety": "passed",
    "encrypted_host_backup": "passed",
    "restore_tamper_and_identity_rejection": "passed",
    "restore_replace_and_rollback": "passed"
  },
  "not_run": {
    "artifact_signing": "not generated by the local builder; this bundle provides checksums, not authenticity",
    "ci_provenance": "not generated by this local harness; GitHub attestations are external to the release manifest",
    "ubuntu_systemd_host": "requires an explicit target host",
    "tailscale_mutation": "intentionally excluded from this static harness",
    "docker_mutation": "intentionally excluded; runtime volumes are not part of the backup contract",
    "physical_host_atomicity": "rename activation is tested on one local filesystem; crash and filesystem behavior require target-host rehearsal",
    "key_custody": "age identity custody is operator policy"
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
