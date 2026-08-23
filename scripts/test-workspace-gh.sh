#!/bin/sh

set -eu

image=${1:-fern/opencode@sha256:73688cd6f96ce3b236bb1c2d25607b03566a4ee92f0fedabeb06fd1a3e643c6c}
volume="fern-gh-smoke-$$"

cleanup() {
	docker volume rm "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

docker image inspect "$image" >/dev/null
version=$(docker run --rm --entrypoint /usr/local/bin/gh "$image" --version)
printf '%s\n' "$version" | grep -q '^gh version 2\.98\.0 '

docker run --rm --entrypoint /bin/sh "$image" -c '
set -eu
test "$(id -u)" = 1001
test -x /usr/local/bin/gh
test -x /usr/bin/git
gh auth login --help >/dev/null
gh auth setup-git --help >/dev/null
gh pr create --help >/dev/null
'

docker volume create "$volume" >/dev/null
mount="type=volume,src=$volume,dst=/home/user/.config/gh"
docker run --rm --mount "$mount" --env GH_CONFIG_DIR=/home/user/.config/gh \
	--entrypoint /usr/local/bin/gh "$image" config set git_protocol https --host github.com
protocol=$(docker run --rm --mount "$mount" --env GH_CONFIG_DIR=/home/user/.config/gh \
	--entrypoint /usr/local/bin/gh "$image" config get git_protocol --host github.com)
test "$protocol" = https

if docker run --rm --mount "$mount" --env GH_CONFIG_DIR=/home/user/.config/gh \
	--entrypoint /usr/local/bin/gh "$image" auth status --hostname github.com >/dev/null 2>&1; then
	printf 'error: fresh gh config unexpectedly contains authentication\n' >&2
	exit 1
fi

printf 'Workspace gh smoke test passed (%s, persistent config, no bundled credential)\n' "$(printf '%s\n' "$version" | sed -n '1p')"
