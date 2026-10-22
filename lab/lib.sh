# shellcheck shell=bash
# Names and helpers the lab scripts share. Source it; don't run it. Sourcing
# it moves to the repository's root. The lab needs bash 5: macOS's
# /bin/bash is 3.2, so put Homebrew's first in PATH.
LAB=fern-lab
LABEL=app.kubernetes.io/part-of=fern
cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit

# GO_RUN starts a container with the repository mounted and Go's module and
# build caches kept in volumes, so nothing is installed on the Mac.
# shellcheck disable=SC2034 # the scripts that source this use it
GO_RUN=(docker run --rm --label "$LABEL" -v "$PWD:/src" -w /src
  -v fern-lab-gomod:/go/pkg/mod -v fern-lab-gocache:/root/.cache/go-build
  -e GOFLAGS=-buildvcs=false)

# lab_up says whether the lab's container exists.
lab_up() { docker inspect "$LAB" >/dev/null 2>&1; }

# k runs kubectl inside the lab.
k() { docker exec -i "$LAB" kubectl "$@"; }

# fern runs the fern CLI in the daemon pod.
fern() { lab/fern "$@"; }

# in_t3 runs a shell command in T3's server container, as T3's user.
in_t3() { k -n fern-system exec -i deploy/t3 -c t3 -- sh -c "$1"; }

# import_images copies images from Docker into the lab's containerd.
import_images() {
  docker save --platform "linux/$(docker version -f '{{.Server.Arch}}')" "$@" |
    docker exec -i "$LAB" ctr -n k8s.io images import - >/dev/null
}

# tar_dir writes a tar of a directory to stdout, without the extended
# attributes and ._ files macOS's tar adds.
tar_dir() { COPYFILE_DISABLE=1 tar -C "$1" --no-xattrs -cf - .; }
