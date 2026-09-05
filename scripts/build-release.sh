#!/bin/sh

set -eu

usage() {
	printf 'usage: %s <version>\n' "$0" >&2
	exit 2
}

[ "$#" -eq 1 ] || usage
version=$1

# A leading "v" is accepted for release tags, while the remainder follows SemVer.
if ! printf '%s\n' "$version" | grep -Eq '^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(\.((0|[1-9][0-9]*)|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'; then
	printf 'error: version must be a semantic version (for example, v0.1.0)\n' >&2
	exit 2
fi

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

export LC_ALL=C
export TZ=UTC
umask 022

commit=$(git rev-parse --verify HEAD)
case "$commit" in
	*[!0-9a-f]*|'')
		printf 'error: could not determine Git commit\n' >&2
		exit 1
		;;
esac
if [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
	printf 'error: release builds require a clean working tree\n' >&2
	exit 1
fi

source_date_epoch=$(git show -s --format=%ct "$commit")
case "$source_date_epoch" in
	*[!0-9]*|'')
		printf 'error: could not determine source commit timestamp\n' >&2
		exit 1
		;;
esac
export SOURCE_DATE_EPOCH=$source_date_epoch

verified_tag=${FERN_VERIFIED_TAG:-}
image_repository=${FERN_IMAGE_REPOSITORY:-fern/opencode-background-source}
image_digest=${FERN_IMAGE_DIGEST:-}
image_sbom_path=${FERN_IMAGE_SBOM_PATH:-}
image_provenance_url=${FERN_IMAGE_PROVENANCE_URL:-}
image_certificate_identity=${FERN_IMAGE_CERTIFICATE_IDENTITY:-}
image_oidc_issuer=${FERN_IMAGE_OIDC_ISSUER:-}

if [ -z "$image_digest" ]; then
	for value in "$verified_tag" "$image_sbom_path" "$image_provenance_url" "$image_certificate_identity" "$image_oidc_issuer"; do
		if [ -n "$value" ]; then
			printf 'error: partial published-image metadata is not allowed\n' >&2
			exit 1
		fi
	done
	[ "$image_repository" = fern/opencode-background-source ] || {
		printf 'error: local release image repository must be fern/opencode-background-source\n' >&2
		exit 1
	}
	version_source=builder-argument
	verified_tag_json=null
	asset_provenance_status=not-generated-local
	image_publication_status=not-published-local
	image_digest_json=null
	image_reference_json=null
	image_sbom_status=not-generated-local
	image_sbom_format_json=null
	image_sbom_asset_json=null
	image_sbom_sha_json=null
	image_sbom_subject_json=null
	image_signature_status=not-generated-local
	image_signature_subject_json=null
	image_certificate_identity_json=null
	image_oidc_issuer_json=null
	image_provenance_status=not-generated-local
	image_provenance_subject_json=null
	image_provenance_type_json=null
	image_provenance_url_json=null
	image_sbom_asset=
else
	[ "$verified_tag" = "$version" ] || {
		printf 'error: verified tag must exactly match the release version\n' >&2
		exit 1
	}
	printf '%s\n' "$image_repository" | grep -Eq '^ghcr\.io/[a-z0-9._/-]+$' || {
		printf 'error: invalid GHCR image repository\n' >&2
		exit 1
	}
	printf '%s\n' "$image_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || {
		printf 'error: invalid image digest\n' >&2
		exit 1
	}
	[ -f "$image_sbom_path" ] && [ ! -L "$image_sbom_path" ] || {
		printf 'error: image SBOM must be a regular file\n' >&2
		exit 1
	}
	image_sbom_asset=$(basename -- "$image_sbom_path")
	printf '%s\n' "$image_sbom_asset" | grep -Eq '^[A-Za-z0-9._-]+\.spdx\.json$' || {
		printf 'error: invalid image SBOM asset name\n' >&2
		exit 1
	}
	printf '%s\n' "$image_certificate_identity" | grep -Eq '^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/release\.yml@refs/tags/v?[0-9A-Za-z.+-]+$' || {
		printf 'error: invalid image certificate identity\n' >&2
		exit 1
	}
	printf '%s\n' "$image_oidc_issuer" | grep -Eq '^https://token\.actions\.githubusercontent\.com$' || {
		printf 'error: invalid image OIDC issuer\n' >&2
		exit 1
	}
	printf '%s\n' "$image_provenance_url" | grep -Eq '^https://github\.com/[A-Za-z0-9_./?&=%:+-]+$' || {
		printf 'error: invalid image provenance URL\n' >&2
		exit 1
	}
	image_reference="$image_repository@$image_digest"
	image_sbom_sha=$(shasum -a 256 "$image_sbom_path" | awk '{print $1}')
	version_source=verified-annotated-tag
	verified_tag_json="\"$verified_tag\""
	asset_provenance_status=verified-github-attestation
	image_publication_status=published
	image_digest_json="\"$image_digest\""
	image_reference_json="\"$image_reference\""
	image_sbom_status=generated-and-attested
	image_sbom_format_json='"spdx-json"'
	image_sbom_asset_json="\"$image_sbom_asset\""
	image_sbom_sha_json="\"$image_sbom_sha\""
	image_sbom_subject_json="\"$image_reference\""
	image_signature_status=verified-keyless
	image_signature_subject_json="\"$image_reference\""
	image_certificate_identity_json="\"$image_certificate_identity\""
	image_oidc_issuer_json="\"$image_oidc_issuer\""
	image_provenance_status=verified-github-attestation
	image_provenance_subject_json="\"$image_reference\""
	image_provenance_type_json='"https://slsa.dev/provenance/v1"'
	image_provenance_url_json="\"$image_provenance_url\""
fi

staging=$(mktemp -d "${TMPDIR:-/tmp}/fern-release.XXXXXX")
payload="$staging/payload"
output="$staging/output"
mkdir -p "$payload" "$output"
backup=
installed=false
cleanup() {
	status=$?
	if [ "$installed" = false ] && [ -n "$backup" ] && [ -e "$backup" ] && [ ! -e dist ]; then
		mv "$backup" dist
	fi
	rm -rf "$staging"
	if [ -n "$backup" ] && [ -e "$backup" ]; then
		rm -rf "$backup"
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

ldflags="-s -w -buildid= -X main.version=$version -X main.commit=$commit"
for arch in amd64 arm64; do
	name="fern-${version}-linux-${arch}"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
		go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$payload/$name" ./cmd/fern
done

mkdir -p "$payload/deploy/systemd" "$payload/deploy/release" "$payload/scripts"
cp deploy/systemd/fern.service deploy/systemd/fern.env.example deploy/systemd/fern.yaml.example \
	"$payload/deploy/systemd/"
cp deploy/release/release-manifest.schema.json \
	deploy/release/backup-manifest.schema.json \
	deploy/release/compatibility-manifest.json \
	deploy/release/compatibility-manifest.schema.json \
	deploy/release/production-rehearsal-evidence.schema.json \
	deploy/release/transaction-manifest.schema.json \
	deploy/release/transaction-manifest.example.json \
	"$payload/deploy/release/"
cp scripts/fern-host-backup.py "$payload/scripts/"
chmod 0755 "$payload/scripts/fern-host-backup.py"

checksum() {
	shasum -a 256 "$payload/$1" | awk '{print $1}'
}
amd64_sha=$(checksum "fern-${version}-linux-amd64")
arm64_sha=$(checksum "fern-${version}-linux-arm64")
service_sha=$(checksum deploy/systemd/fern.service)
env_sha=$(checksum deploy/systemd/fern.env.example)
config_sha=$(checksum deploy/systemd/fern.yaml.example)
release_schema_sha=$(checksum deploy/release/release-manifest.schema.json)
backup_schema_sha=$(checksum deploy/release/backup-manifest.schema.json)
compatibility_manifest_sha=$(checksum deploy/release/compatibility-manifest.json)
compatibility_schema_sha=$(checksum deploy/release/compatibility-manifest.schema.json)
rehearsal_schema_sha=$(checksum deploy/release/production-rehearsal-evidence.schema.json)
transaction_schema_sha=$(checksum deploy/release/transaction-manifest.schema.json)
transaction_example_sha=$(checksum deploy/release/transaction-manifest.example.json)
backup_utility_sha=$(checksum scripts/fern-host-backup.py)
go_version=$(go env GOVERSION)
bundle_asset="fern-${version}-bundle.tar.gz"
bundle_root="fern-${version}"

cat >"$payload/RELEASE-MANIFEST.json" <<EOF
{
  "schema_version": 2,
  "release": {
    "version": "$version",
    "commit": "$commit",
    "source_date_epoch": $source_date_epoch,
    "version_source": "$version_source",
    "verified_tag": $verified_tag_json
  },
  "build": {
    "go_version": "$go_version",
    "cgo_enabled": false,
    "trimpath": true,
    "vcs_stamping": false
  },
  "integrity": {
    "checksum_algorithm": "sha256",
    "binary_signature_status": "not-generated",
    "asset_provenance_status": "$asset_provenance_status"
  },
  "image": {
    "repository": "$image_repository",
    "publication_status": "$image_publication_status",
    "digest": $image_digest_json,
    "reference": $image_reference_json,
    "sbom": {
      "status": "$image_sbom_status",
      "format": $image_sbom_format_json,
      "asset": $image_sbom_asset_json,
      "sha256": $image_sbom_sha_json,
      "attestation_subject": $image_sbom_subject_json
    },
    "signature": {
      "status": "$image_signature_status",
      "subject": $image_signature_subject_json,
      "certificate_identity": $image_certificate_identity_json,
      "oidc_issuer": $image_oidc_issuer_json
    },
    "provenance": {
      "status": "$image_provenance_status",
      "subject": $image_provenance_subject_json,
      "predicate_type": $image_provenance_type_json,
      "attestation_url": $image_provenance_url_json
    }
  },
  "distribution": {
    "bundle_asset": "$bundle_asset",
    "bundle_root": "$bundle_root",
    "bundle_checksum_file": "SHA256SUMS",
    "release_checksum_file": "SHA256SUMS"
  },
  "artifacts": [
    {"path": "fern-${version}-linux-amd64", "os": "linux", "arch": "amd64", "sha256": "$amd64_sha"},
    {"path": "fern-${version}-linux-arm64", "os": "linux", "arch": "arm64", "sha256": "$arm64_sha"}
  ],
  "deployment_files": [
    {"path": "deploy/systemd/fern.env.example", "sha256": "$env_sha"},
    {"path": "deploy/systemd/fern.service", "sha256": "$service_sha"},
    {"path": "deploy/systemd/fern.yaml.example", "sha256": "$config_sha"},
    {"path": "deploy/release/release-manifest.schema.json", "sha256": "$release_schema_sha"},
    {"path": "deploy/release/backup-manifest.schema.json", "sha256": "$backup_schema_sha"},
    {"path": "deploy/release/compatibility-manifest.json", "sha256": "$compatibility_manifest_sha"},
    {"path": "deploy/release/compatibility-manifest.schema.json", "sha256": "$compatibility_schema_sha"},
    {"path": "deploy/release/production-rehearsal-evidence.schema.json", "sha256": "$rehearsal_schema_sha"},
    {"path": "deploy/release/transaction-manifest.example.json", "sha256": "$transaction_example_sha"},
    {"path": "deploy/release/transaction-manifest.schema.json", "sha256": "$transaction_schema_sha"},
    {"path": "scripts/fern-host-backup.py", "sha256": "$backup_utility_sha"}
  ],
  "upgrade_rollback": {
    "transaction_manifest_schema": "deploy/release/transaction-manifest.schema.json",
    "transaction_example": "deploy/release/transaction-manifest.example.json",
    "transaction_receipt": "generated-at-restore-target/TRANSACTION-MANIFEST.json",
    "compatibility_manifest": "deploy/release/compatibility-manifest.json",
    "first_supported_baseline": "baseline-v1-repository-established-not-historical-release",
    "upgrade_harness": "integration/upgrade/run.sh",
    "host_utility": "scripts/fern-host-backup.py",
    "support_status": "installed-cli-operational-recovery",
    "activation_model": "staged-filesystem-rollback",
    "credential_policy": "external-recipient-with-checksums",
    "volume_export_mode": "no-runtime-volumes"
  }
}
EOF

(
	cd "$payload"
	find . -type f ! -name SHA256SUMS -print | LC_ALL=C sort | sed 's#^./##' | \
		while IFS= read -r artifact; do
			shasum -a 256 "$artifact"
		done >SHA256SUMS
	shasum -a 256 -c SHA256SUMS >/dev/null
)

python3 scripts/create-release-bundle.py "$payload" "$output/$bundle_asset" "$bundle_root" "$source_date_epoch"
cp "$payload/fern-${version}-linux-amd64" "$payload/fern-${version}-linux-arm64" \
	"$payload/RELEASE-MANIFEST.json" "$output/"
if [ -n "$image_sbom_asset" ]; then
	cp "$image_sbom_path" "$output/$image_sbom_asset"
fi
(
	cd "$output"
	find . -maxdepth 1 -type f ! -name SHA256SUMS -print | LC_ALL=C sort | sed 's#^./##' | \
		while IFS= read -r artifact; do
			shasum -a 256 "$artifact"
		done >SHA256SUMS
	shasum -a 256 -c SHA256SUMS >/dev/null
)

if [ "$(git rev-parse --verify HEAD)" != "$commit" ] || [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
	printf 'error: source tree changed during release build\n' >&2
	exit 1
fi

if [ -e dist ]; then
	backup=".dist.backup.$$"
	[ ! -e "$backup" ] || {
		printf 'error: backup path already exists: %s\n' "$backup" >&2
		exit 1
	}
	mv dist "$backup"
fi
mv "$output" dist
installed=true
if [ -n "$backup" ]; then
	rm -rf "$backup"
	backup=
fi

printf 'built fern %s (%s) in %s/dist\n' "$version" "$commit" "$repo_root"
