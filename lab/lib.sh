# shellcheck shell=bash
# Names and helpers the lab scripts share. Source it; don't run it. Sourcing
# it moves to the repository's root. The lab needs bash 5: macOS's
# /bin/bash is 3.2, so put Homebrew's first in PATH.
#
# With FERN_HOST set, the scripts work on that VPS (lab/vps.sh), as root over
# SSH, instead of the local lab's container.
LAB=fern-lab
LABEL=app.kubernetes.io/part-of=fern
FERN_HOST=${FERN_HOST:-}
# The versions the lab and the VPS run.
# shellcheck disable=SC2034 # the scripts that source this use them
K3S_VERSION=v1.36.5+k3s1 SANDBOX_VERSION=v1.0.7 GVISOR_RELEASE=20261012
# One SSH connection serves a script's many commands.
SSH=(ssh -o BatchMode=yes -o ControlMaster=auto -o "ControlPath=$HOME/.ssh/fern-%C" -o ControlPersist=60)
cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit

# GO_RUN starts a container with the repository mounted and Go's module and
# build caches kept in volumes, so nothing is installed on the Mac.
# shellcheck disable=SC2034 # the scripts that source this use it
GO_RUN=(docker run --rm --label "$LABEL" -v "$PWD:/src" -w /src
  -v fern-lab-gomod:/go/pkg/mod -v fern-lab-gocache:/root/.cache/go-build
  -e GOFLAGS=-buildvcs=false)

# lab_up says whether the lab's container exists, or the VPS answers.
lab_up() {
  if [ -n "$FERN_HOST" ]; then
    "${SSH[@]}" "root@$FERN_HOST" true
  else
    docker inspect "$LAB" >/dev/null 2>&1
  fi
}

# on_node runs a command as root on the node, with stdin; "on_node -it"
# gives it a terminal ("on_node -i" is the default).
on_node() {
  local tty=-i
  case $1 in -i | -it) tty=$1 && shift ;; esac
  if [ -z "$FERN_HOST" ]; then
    docker exec "$tty" "$LAB" "$@"
  elif [ "$tty" = -it ]; then
    "${SSH[@]}" -t "root@$FERN_HOST" "$(printf '%q ' "$@")"
  else
    "${SSH[@]}" "root@$FERN_HOST" "$(printf '%q ' "$@")"
  fi
}

# k runs kubectl on the node.
k() { on_node kubectl "$@"; }

# fern runs the fern CLI in the daemon pod.
fern() { lab/fern "$@"; }

# in_t3 runs a shell command in T3's server container, as T3's user.
in_t3() { k -n fern-system exec -i deploy/t3 -c t3 -- sh -c "$1"; }

# import_images copies images from Docker into the node's containerd.
import_images() {
  local arch
  case $(on_node uname -m </dev/null) in aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
  docker save --platform "linux/$arch" "$@" | on_node ctr -n k8s.io images import - >/dev/null
}

# tar_dir writes a tar of a directory to stdout, without the extended
# attributes and ._ files macOS's tar adds.
tar_dir() { COPYFILE_DISABLE=1 tar -C "$1" --no-xattrs -cf - .; }
