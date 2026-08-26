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
cp "$ROOT/scripts/build-release.sh" "$ROOT/scripts/fern-host-backup.py" "$FIXTURE/scripts/"
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
    "provenance_status": "not-generated-by-builder",
    "ci_attestations": "external-to-manifest",
}
assert manifest["upgrade_rollback"] == {
    "transaction_manifest_schema": "deploy/release/transaction-manifest.schema.json",
    "transaction_example": "deploy/release/transaction-manifest.example.json",
    "transaction_receipt": "generated-at-restore-target/TRANSACTION-MANIFEST.json",
    "compatibility_manifest": "deploy/release/compatibility-manifest.json",
    "first_supported_baseline": "baseline-v1-repository-established-not-historical-release",
    "upgrade_harness": "integration/upgrade/run.sh",
    "host_utility": "scripts/fern-host-backup.py",
    "support_status": "installed-cli-operational-recovery",
    "activation_model": "staged-filesystem-docker-best-effort-rollback",
    "credential_policy": "external-recipient-with-checksums",
    "volume_export_mode": "managed-docker-volume-staged-and-verified",
}
for entry in manifest["artifacts"] + manifest["deployment_files"]:
    actual = hashlib.sha256((root / "dist" / entry["path"]).read_bytes()).hexdigest()
    assert actual == entry["sha256"], entry["path"]

for path in root.glob("deploy/release/*.json"):
    json.loads(path.read_text())
compatibility = json.loads((root / "deploy/release/compatibility-manifest.json").read_text())
assert compatibility["schema_version"] == 1
assert compatibility["first_supported_baseline"]["id"] == "baseline-v1"
assert compatibility["first_supported_baseline"]["status"] == "repository-established"
assert compatibility["first_supported_baseline"]["historical_release"] is False
assert compatibility["first_supported_baseline"]["historical_tag"] is None
assert compatibility["first_supported_baseline"]["task_store_schema"] == 4
assert compatibility["current_release_schemas"]["task_store"] == 5
transaction = json.loads((root / "deploy/release/transaction-manifest.example.json").read_text())
assert transaction["backup"]["format"] == "fern-host-backup-v1"
assert transaction["activation"]["model"] == "staged-current-previous"
assert transaction["rollback"]["available"] is True
assert "PLACEHOLDER" not in json.dumps(transaction)
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

BACKUP="$ROOT/scripts/fern-host-backup.py"
HOST="$TEMP/host"
LOCK="$HOST/lock"
SOURCE="$HOST/source"
mkdir -p "$SOURCE/state/.fern/control" "$SOURCE/state/.config/gh" \
  "$SOURCE/state/.fern/github-app" "$SOURCE/config" "$SOURCE/repository/.git" \
  "$SOURCE/volume/sessions" "$SOURCE/gh-volume"
printf 'state-a\n' >"$SOURCE/state/.fern/control/state.db"
printf 'oauth_token: secret-gh-token\n' >"$SOURCE/state/.config/gh/hosts.yml"
printf '{"client_secret":"secret-app","private_key":"secret-private-key"}\n' >"$SOURCE/state/.fern/github-app/app-credentials.json"
printf 'proxy: safe\n' >"$SOURCE/config/fern.yaml"
printf 'OPENCODE_PASSWORD=secret\n' >"$SOURCE/config/fern.env"
printf 'repository-a\n' >"$SOURCE/repository/work.txt"
printf 'git-config\n' >"$SOURCE/repository/.git/config"
printf 'volume-secret-a\n' >"$SOURCE/volume/sessions/auth.json"
printf 'oauth_token: volume-gh-token\n' >"$SOURCE/gh-volume/hosts.yml"

python3 "$BACKUP" init-epoch --lock-dir "$LOCK" --epoch appliance-A
python3 "$BACKUP" backup --lock-dir "$LOCK" --epoch appliance-A \
  --generation generation-a --output "$HOST/backup-a" \
  --state "$SOURCE/state" --config "$SOURCE/config" --repository "$SOURCE/repository" \
  --volume fern-demo-v2-data="$SOURCE/volume" \
  --volume fern-demo-v1-gh-config="$SOURCE/gh-volume" --credential-policy external \
  --credential-output "$HOST/credentials-a.tar"
python3 "$BACKUP" backup --lock-dir "$LOCK" --epoch appliance-A \
  --generation generation-a --output "$HOST/backup-a-copy" \
  --state "$SOURCE/state" --config "$SOURCE/config" --repository "$SOURCE/repository" \
  --volume fern-demo-v2-data="$SOURCE/volume" \
  --volume fern-demo-v1-gh-config="$SOURCE/gh-volume" --credential-policy external \
  --credential-output "$HOST/credentials-a-copy.tar"
diff -r "$HOST/backup-a" "$HOST/backup-a-copy" >"$TEMP/backup-reproducibility.diff"
cmp "$HOST/credentials-a.tar" "$HOST/credentials-a-copy.tar"

python3 - "$HOST/backup-a/BACKUP-MANIFEST.json" <<'PY'
import json, pathlib, sys
manifest = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert manifest["named_volumes"] == ["fern-demo-v2-data", "fern-demo-v1-gh-config"]
assert manifest["credentials"]["workspace_gh"] == "included-in-external-recipient"
assert manifest["credentials"]["detected_entries"] == 6
assert manifest["credentials"]["general_archive_contains_detected_plaintext_credentials"] is False
assert manifest["credentials"]["external"]["sha256"]
for component in manifest["components"]:
    assert component["entries"]
    assert all(entry["sha256"] for entry in component["entries"])
PY
! grep -R -a -q 'secret-gh-token\|secret-app\|secret-private-key\|OPENCODE_PASSWORD\|volume-secret\|volume-gh-token' "$HOST/backup-a"
grep -a -q 'secret-app' "$HOST/credentials-a.tar"
grep -a -q 'volume-gh-token' "$HOST/credentials-a.tar"

mkdir -p "$HOST/hardlink-source/state" "$HOST/hardlink-source/config" "$HOST/hardlink-source/repository"
printf 'linked-secret\n' >"$HOST/hardlink-source/state/credentials.json"
ln "$HOST/hardlink-source/state/credentials.json" "$HOST/hardlink-source/repository/ordinary.txt"
if python3 "$BACKUP" backup --lock-dir "$LOCK" --epoch appliance-A \
  --generation hardlink --output "$HOST/hardlink-backup" \
  --state "$HOST/hardlink-source/state" --config "$HOST/hardlink-source/config" \
  --repository "$HOST/hardlink-source/repository" --credential-policy exclude \
  >"$TEMP/hardlink-rejection.txt" 2>&1; then
  printf 'error: backup accepted a hard-linked credential alias\n' >&2
  exit 1
fi
grep -q 'hard-linked file rejected' "$TEMP/hardlink-rejection.txt"

cp -R "$HOST/backup-a" "$HOST/tampered"
printf 'corruption\n' >>"$HOST/tampered/state.tar"
if python3 "$BACKUP" restore --lock-dir "$LOCK" --epoch appliance-A \
  --backup "$HOST/tampered" --target "$HOST/tampered-target" \
  --credential-input "$HOST/credentials-a.tar" >"$TEMP/backup-tamper.txt" 2>&1; then
  printf 'error: restore accepted a checksum-tampered backup\n' >&2
  exit 1
fi
grep -q 'checksum mismatch' "$TEMP/backup-tamper.txt"

make_malicious_backup() {
  local kind=$1 destination=$2
  cp -R "$HOST/backup-a" "$destination"
  python3 - "$destination" "$kind" <<'PY'
import hashlib, io, json, pathlib, tarfile, sys
root = pathlib.Path(sys.argv[1])
kind = sys.argv[2]
manifest_path = root / "BACKUP-MANIFEST.json"
manifest = json.loads(manifest_path.read_text())
component = next(item for item in manifest["components"] if item["name"] == "repository")
entry = component["entries"][0]
with tarfile.open(root / component["archive"], "w") as archive:
    info = tarfile.TarInfo("../escaped" if kind == "escape" else entry["path"])
    if kind == "symlink":
        info.type = tarfile.SYMTYPE
        info.linkname = "/etc/passwd"
        archive.addfile(info)
    else:
        data = b"escape"
        info.size = len(data)
        archive.addfile(info, io.BytesIO(data))
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
component["sha256"] = sha(root / component["archive"])
manifest_path.write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n")
names = sorted(path.name for path in root.iterdir() if path.is_file() and path.name != "SHA256SUMS")
(root / "SHA256SUMS").write_text("".join(f"{sha(root / name)}  {name}\n" for name in names))
PY
}

make_malicious_backup symlink "$HOST/symlink-backup"
if python3 "$BACKUP" restore --lock-dir "$LOCK" --epoch appliance-A \
  --backup "$HOST/symlink-backup" --target "$HOST/symlink-target" \
  --credential-input "$HOST/credentials-a.tar" >"$TEMP/symlink-rejection.txt" 2>&1; then
  printf 'error: restore accepted a symlink archive entry\n' >&2
  exit 1
fi
grep -q 'link or special archive entry rejected' "$TEMP/symlink-rejection.txt"

make_malicious_backup escape "$HOST/escape-backup"
if python3 "$BACKUP" restore --lock-dir "$LOCK" --epoch appliance-A \
  --backup "$HOST/escape-backup" --target "$HOST/escape-target" \
  --credential-input "$HOST/credentials-a.tar" >"$TEMP/escape-rejection.txt" 2>&1; then
  printf 'error: restore accepted a path-escape archive entry\n' >&2
  exit 1
fi
grep -q 'unsafe or duplicate archive path' "$TEMP/escape-rejection.txt"
test ! -e "$HOST/escaped"

rm -rf "$SOURCE"
python3 "$BACKUP" restore --lock-dir "$LOCK" --epoch appliance-A \
  --backup "$HOST/backup-a" --target "$HOST/restored" \
  --credential-input "$HOST/credentials-a.tar"
grep -qx 'repository-a' "$HOST/restored/current/repository/work.txt"
grep -q 'secret-gh-token' "$HOST/restored/current/state/.config/gh/hosts.yml"
grep -q 'volume-secret-a' "$HOST/restored/current/volumes/fern-demo-v2-data/sessions/auth.json"
grep -q 'volume-gh-token' "$HOST/restored/current/volumes/fern-demo-v1-gh-config/hosts.yml"
grep -qx 'appliance-A' "$HOST/restored/current/.fern-appliance-epoch"
python3 - "$HOST/restored/TRANSACTION-MANIFEST.json" <<'PY'
import json, pathlib, sys
manifest = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert manifest["operation"] == "restore"
assert manifest["phase"] == "activated"
assert manifest["generation"] == manifest["activation"]["current_generation"] == "generation-a"
assert manifest["rollback"] == {"available": False, "previous_generation": None}
PY

SOURCE_B="$HOST/source-b"
mkdir "$SOURCE_B"
cp -R "$HOST/restored/current/state" "$HOST/restored/current/config" \
  "$HOST/restored/current/repository" "$HOST/restored/current/volumes" "$SOURCE_B/"
printf 'repository-b\n' >"$SOURCE_B/repository/work.txt"
printf 'volume-secret-b\n' >"$SOURCE_B/volumes/fern-demo-v2-data/sessions/auth.json"
python3 "$BACKUP" backup --lock-dir "$LOCK" --epoch appliance-A \
  --generation generation-b --output "$HOST/backup-b" \
  --state "$SOURCE_B/state" --config "$SOURCE_B/config" \
  --repository "$SOURCE_B/repository" \
  --volume fern-demo-v2-data="$SOURCE_B/volumes/fern-demo-v2-data" \
  --volume fern-demo-v1-gh-config="$SOURCE_B/volumes/fern-demo-v1-gh-config" \
  --credential-policy external --credential-output "$HOST/credentials-b.tar"
python3 "$BACKUP" restore --lock-dir "$LOCK" --epoch appliance-A \
  --backup "$HOST/backup-b" --target "$HOST/restored" \
  --credential-input "$HOST/credentials-b.tar"
grep -qx 'repository-b' "$HOST/restored/current/repository/work.txt"
python3 "$BACKUP" rollback --lock-dir "$LOCK" --epoch appliance-A --target "$HOST/restored"
grep -qx 'repository-a' "$HOST/restored/current/repository/work.txt"
grep -qx 'generation-a' "$HOST/restored/current/.fern-generation"
grep -q '"phase": "rolled-back"' "$HOST/restored/TRANSACTION-MANIFEST.json"

if python3 "$BACKUP" restore --lock-dir "$LOCK" --epoch appliance-old \
  --backup "$HOST/backup-a" --target "$HOST/old-epoch-target" \
  --credential-input "$HOST/credentials-a.tar" >"$TEMP/epoch-rejection.txt" 2>&1; then
  printf 'error: restore accepted an old appliance epoch\n' >&2
  exit 1
fi
grep -q 'appliance epoch mismatch' "$TEMP/epoch-rejection.txt"

mkdir "$LOCK/operator.lock"
if python3 "$BACKUP" backup --lock-dir "$LOCK" --epoch appliance-A \
  --generation locked --output "$HOST/locked-backup" \
  --state "$HOST/restored/current/state" --config "$HOST/restored/current/config" \
  --repository "$HOST/restored/current/repository" --credential-policy exclude \
  >"$TEMP/lock-rejection.txt" 2>&1; then
  printf 'error: backup ignored the exclusive operator lock\n' >&2
  exit 1
fi
rmdir "$LOCK/operator.lock"
grep -q 'operator lock is already held' "$TEMP/lock-rejection.txt"

python3 "$BACKUP" backup --lock-dir "$LOCK" --epoch appliance-A \
  --generation generation-excluded --output "$HOST/backup-excluded" \
  --state "$HOST/restored/current/state" --config "$HOST/restored/current/config" \
  --repository "$HOST/restored/current/repository" --credential-policy exclude
python3 - "$HOST/backup-excluded/BACKUP-MANIFEST.json" <<'PY'
import json, pathlib, sys
manifest = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert manifest["credentials"]["policy"] == "exclude"
assert manifest["credentials"]["workspace_gh"] == "excluded-reauthorize"
assert manifest["credentials"]["external"] is None
PY
python3 "$BACKUP" restore --lock-dir "$LOCK" --epoch appliance-A \
  --backup "$HOST/backup-excluded" --target "$HOST/excluded-restore"
test ! -e "$HOST/excluded-restore/current/state/.config/gh/hosts.yml"
test ! -e "$HOST/excluded-restore/current/config/fern.env"
grep -qx 'repository-a' "$HOST/excluded-restore/current/repository/work.txt"

NEW_LOCK="$HOST/replacement-lock"
python3 "$BACKUP" init-epoch --lock-dir "$NEW_LOCK" --epoch appliance-B
python3 "$BACKUP" restore --lock-dir "$NEW_LOCK" --epoch appliance-B \
  --backup "$HOST/backup-a" --target "$HOST/replacement-restore" \
  --credential-input "$HOST/credentials-a.tar"
grep -qx 'appliance-B' "$HOST/replacement-restore/current/.fern-appliance-epoch"
grep -qx 'appliance-A' "$HOST/replacement-restore/current/.fern-source-epoch"
if python3 "$BACKUP" restore --lock-dir "$LOCK" --epoch appliance-A \
  --backup "$HOST/backup-a" --target "$HOST/replacement-restore" \
  --credential-input "$HOST/credentials-a.tar" >"$TEMP/target-epoch-rejection.txt" 2>&1; then
  printf 'error: restore replaced a generation owned by another appliance epoch\n' >&2
  exit 1
fi
grep -q 'generation is fenced by another appliance epoch' "$TEMP/target-epoch-rejection.txt"

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
cp "$TEMP/backup-tamper.txt" "$EVIDENCE/backup-tamper-rejection.txt"
cp "$TEMP/symlink-rejection.txt" "$EVIDENCE/symlink-rejection.txt"
cp "$TEMP/escape-rejection.txt" "$EVIDENCE/path-escape-rejection.txt"
cp "$TEMP/epoch-rejection.txt" "$EVIDENCE/epoch-rejection.txt"
cp "$TEMP/target-epoch-rejection.txt" "$EVIDENCE/target-epoch-rejection.txt"
cp "$TEMP/hardlink-rejection.txt" "$EVIDENCE/hardlink-rejection.txt"
cat >"$EVIDENCE/static-assertions.txt" <<'EOF'
PASS systemd runs as fern with explicit hardening and distinct remote/operator loopback listeners
PASS systemd contains no Docker/Tailscale destructive lifecycle command
PASS deployment assets contain no wildcard Fern listener
PASS deployment assets identify only the remote loopback listener for Tailscale Serve
PASS deployment configuration requires an exact HTTPS remote origin replacement
PASS deployment runbook contains no executable Tailscale Funnel command
PASS deterministic host backup segregates detected credentials and explicit named-volume exports
PASS restore rejects checksum tampering, symlinks, path escapes, hardlinks, stale appliance epochs, and cross-epoch targets
PASS destructive restore activation retains and rolls back to the previous generation
PASS restore and rollback emit transaction manifests matching the active generation
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
    "deterministic_host_backup": "passed",
    "credential_and_volume_segregation": "passed",
    "restore_tamper_and_path_safety": "passed",
    "destructive_restore_and_rollback": "passed",
    "operator_lock_and_epoch_fencing": "passed"
  },
  "not_run": {
    "artifact_signing": "not generated by the local builder; this bundle provides checksums, not authenticity",
    "ci_provenance": "not generated by this local harness; GitHub attestations are external to the release manifest",
    "ubuntu_systemd_host": "requires an explicit target host",
    "tailscale_mutation": "intentionally excluded from this static harness",
    "docker_mutation": "intentionally excluded; named volume fixtures are pre-exported directories",
    "physical_host_atomicity": "rename activation is tested on one local filesystem; crash and filesystem behavior require target-host rehearsal",
    "credential_encryption": "external recipient segregation is tested; encryption and custody are operator policy"
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
