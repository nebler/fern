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

commit=$(git rev-parse --verify HEAD)
case "$commit" in
	*[!0-9a-f]*|'')
		printf 'error: could not determine Git commit\n' >&2
		exit 1
		;;
esac

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
trap cleanup EXIT HUP INT TERM

ldflags="-s -w -buildid= -X main.version=$version -X main.commit=$commit"
for arch in amd64 arm64; do
	name="fern-${version}-linux-${arch}"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
		go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$staging/$name" ./cmd/fern
done

(
	cd "$staging"
	LC_ALL=C shasum -a 256 fern-* > SHA256SUMS.local
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
