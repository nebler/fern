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

staging=$(mktemp -d "${TMPDIR:-/tmp}/fern-release.XXXXXX")
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
		go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$staging/$name" ./cmd/fern
done

mkdir -p "$staging/deploy/systemd" "$staging/deploy/release"
cp deploy/systemd/fern.service deploy/systemd/fern.env.example deploy/systemd/fern.yaml.example \
	"$staging/deploy/systemd/"
cp deploy/release/release-manifest.schema.json \
	deploy/release/transaction-manifest.schema.json \
	deploy/release/transaction-manifest.example.json \
	"$staging/deploy/release/"

amd64_sha=$(shasum -a 256 "$staging/fern-${version}-linux-amd64" | awk '{print $1}')
arm64_sha=$(shasum -a 256 "$staging/fern-${version}-linux-arm64" | awk '{print $1}')
service_sha=$(shasum -a 256 "$staging/deploy/systemd/fern.service" | awk '{print $1}')
env_sha=$(shasum -a 256 "$staging/deploy/systemd/fern.env.example" | awk '{print $1}')
config_sha=$(shasum -a 256 "$staging/deploy/systemd/fern.yaml.example" | awk '{print $1}')
release_schema_sha=$(shasum -a 256 "$staging/deploy/release/release-manifest.schema.json" | awk '{print $1}')
transaction_schema_sha=$(shasum -a 256 "$staging/deploy/release/transaction-manifest.schema.json" | awk '{print $1}')
transaction_example_sha=$(shasum -a 256 "$staging/deploy/release/transaction-manifest.example.json" | awk '{print $1}')
go_version=$(go env GOVERSION)

cat >"$staging/RELEASE-MANIFEST.json" <<EOF
{
  "schema_version": 1,
  "release": {
    "version": "$version",
    "commit": "$commit",
    "source_date_epoch": $source_date_epoch,
    "version_source": "builder-argument"
  },
  "build": {
    "go_version": "$go_version",
    "cgo_enabled": false,
    "trimpath": true,
    "vcs_stamping": false
  },
  "integrity": {
    "checksum_algorithm": "sha256",
    "signature_status": "not-generated"
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
    {"path": "deploy/release/transaction-manifest.example.json", "sha256": "$transaction_example_sha"},
    {"path": "deploy/release/transaction-manifest.schema.json", "sha256": "$transaction_schema_sha"}
  ],
  "upgrade_rollback": {
    "transaction_manifest": "deploy/release/transaction-manifest.example.json",
    "support_status": "not-implemented",
    "sqlite_backup": "placeholder-only-not-implemented",
    "application_secrets_backup": "placeholder-only-not-implemented"
  }
}
EOF

(
	cd "$staging"
	find . -type f ! -name SHA256SUMS ! -name SHA256SUMS.local -print | LC_ALL=C sort | sed 's#^./##' | \
		while IFS= read -r artifact; do
			shasum -a 256 "$artifact"
		done > SHA256SUMS.local
	shasum -a 256 -c SHA256SUMS.local >/dev/null
	sed 's#  #  dist/#' SHA256SUMS.local > SHA256SUMS
	rm SHA256SUMS.local
)

if [ -e dist ]; then
	backup=".dist.backup.$$"
	[ ! -e "$backup" ] || {
		printf 'error: backup path already exists: %s\n' "$backup" >&2
		exit 1
	}
	mv dist "$backup"
fi
mv "$staging" dist
installed=true
if [ -n "$backup" ]; then
	rm -rf "$backup"
	backup=
fi

printf 'built fern %s (%s) in %s/dist\n' "$version" "$commit" "$repo_root"
