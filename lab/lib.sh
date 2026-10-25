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
SSH=(ssh -o BatchMode=yes -o LogLevel=ERROR -o ControlMaster=auto -o "ControlPath=$HOME/.ssh/fern-%C" -o ControlPersist=60)
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
fern() {
  case " $* " in
    *" --repo "* | *" -it "*) lab/fern "$@" ;; # a local repo to send, or a terminal
    *) on_node "$FERN_CLI" "$@" ;;
  esac
}

# FERN_CLI is the fern CLI on the node, a copy of the image's client binary
# (install_cli). It talks to the daemon's socket, which is on the node, so a
# command doesn't go through kubectl exec (about 110 ms a command).
FERN_CLI=/usr/local/bin/fern-cli

# install_cli copies the fern binary from the fern:dev image to the node,
# unless the node has the same one.
install_cli() {
  local tmp cid
  tmp=$(mktemp -d)
  cid=$(docker create --label "$LABEL" fern:dev)
  docker cp -q "$cid:/usr/local/bin/fern" "$tmp/fern"
  docker rm "$cid" >/dev/null
  if [ "$(on_node sh -c "sha256sum $FERN_CLI 2>/dev/null | cut -d' ' -f1" </dev/null)" != "$(shasum -a 256 "$tmp/fern" | cut -d' ' -f1)" ]; then
    gzip -c "$tmp/fern" | on_node sh -c "gunzip > $FERN_CLI.new && chmod 755 $FERN_CLI.new && mv $FERN_CLI.new $FERN_CLI"
  fi
  rm -rf "$tmp"
}

# in_t3 runs a shell command in T3's server container, as T3's user.
in_t3() { k -n fern-system exec -i deploy/t3 -c t3 -- sh -c "$1"; }

# import_images copies images from Docker into the node's containerd,
# skipping those it has: the node keeps the Docker ID of each it imported,
# since a VPS is far away for gigabytes.
import_images() {
  local arch image id changed=() ids=()
  case $(on_node uname -m </dev/null) in aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
  for image in "$@"; do
    id=$(docker image inspect -f '{{.Id}}' "$image")
    if [ "$(on_node sh -c "crictl inspecti -q docker.io/library/$image >/dev/null 2>&1 &&
      cat /var/lib/fern-images/${image/:/-}" </dev/null 2>/dev/null)" != "$id" ]; then
      changed+=("$image")
      ids+=("$id")
    fi
  done
  [ ${#changed[@]} -gt 0 ] || return 0
  docker save --platform "linux/$arch" "${changed[@]}" | on_node ctr -n k8s.io images import - >/dev/null
  for i in "${!changed[@]}"; do
    echo "${ids[$i]}" | on_node sh -c "mkdir -p /var/lib/fern-images && cat >/var/lib/fern-images/${changed[$i]/:/-}"
  done
}

# tar_dir writes a tar of a directory to stdout, without the extended
# attributes and ._ files macOS's tar adds.
tar_dir() { COPYFILE_DISABLE=1 tar -C "$1" --no-xattrs -cf - .; }

# lab_lock waits until no other script uses the lab (or FERN_HOST's VPS),
# then holds it until this script and its children end: worktrees share
# the lab, the images' :dev tags and Docker, and two builds or check runs
# at once overwrite each other's images and skew each other's timings. A
# lock whose owner is gone is taken over, so a killed script leaves none.
lab_lock() {
  [ -z "${FERN_LAB_LOCKED:-}" ] || return 0
  local lock=${TMPDIR:-/tmp}/fern-lab-${FERN_HOST:-local}.lock owner said=""
  until mkdir "$lock" 2>/dev/null; do
    owner=$(cat "$lock/owner" 2>/dev/null || true)
    if [ -n "$owner" ] && ! kill -0 "${owner%% *}" 2>/dev/null; then
      rm -rf "$lock"
      continue
    fi
    [ -n "$said" ] || { echo "waiting for the lab, held by: ${owner#* }" >&2; said=1; }
    sleep 1
  done
  echo "$$ $(basename "$0") in $PWD" >"$lock/owner"
  export FERN_LAB_LOCKED=$lock
}
