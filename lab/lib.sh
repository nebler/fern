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
# The gVisor release the lab and the VPS run.
# shellcheck disable=SC2034 # the scripts that source this use it
GVISOR_RELEASE=20261012
# One SSH connection serves a script's many commands.
SSH=(ssh -o BatchMode=yes -o LogLevel=ERROR -o ControlMaster=auto -o "ControlPath=$HOME/.ssh/fern-%C" -o ControlPersist=60)
cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit

# GO_RUN starts a container with the repository mounted and Go's module and
# build caches kept in volumes, so nothing is installed on the Mac. CI sets
# FERN_GOMODCACHE and FERN_GOCACHE to folders on the runner, which it caches.
# shellcheck disable=SC2034 # the scripts that source this use it
GO_RUN=(docker run --rm --label "$LABEL" -v "$PWD:/src" -w /src
  -v "${FERN_GOMODCACHE:-fern-lab-gomod}:/go/pkg/mod" -v "${FERN_GOCACHE:-fern-lab-gocache}:/root/.cache/go-build"
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

# fern runs the fern CLI on the node.
fern() {
  case " $* " in
    *" --repo "* | *" -it "*) lab/fern "$@" ;; # a local repo to send, or a terminal
    *) on_node "$FERN_CLI" "$@" ;;
  esac
}

# FERN_CLI is the fern CLI on the node (install_cli), beside fern-daemon,
# which fern-daemon.service runs.
FERN_CLI=/usr/local/bin/fern

# install_cli copies the fern and fern-daemon binaries from the fern:dev
# image to the node, unless it has the same ones.
install_cli() {
  local tmp cid bin
  tmp=$(mktemp -d)
  cid=$(docker create --label "$LABEL" fern:dev)
  for bin in fern fern-daemon; do
    docker cp -q "$cid:/usr/local/bin/$bin" "$tmp/$bin"
    if [ "$(on_node sh -c "sha256sum /usr/local/bin/$bin 2>/dev/null | cut -d' ' -f1" </dev/null)" != "$(shasum -a 256 "$tmp/$bin" | cut -d' ' -f1)" ]; then
      gzip -c "$tmp/$bin" | on_node sh -c "gunzip > /usr/local/bin/$bin.new && chmod 755 /usr/local/bin/$bin.new && mv /usr/local/bin/$bin.new /usr/local/bin/$bin"
    fi
  done
  docker rm "$cid" >/dev/null
  rm -rf "$tmp"
}

# install_units installs Fern's systemd units (deploy/systemd) on the node
# and enables them: fern-daemon, and fern-t3 with "t3".
install_units() {
  local unit units=(fern-daemon)
  [ "${1:-}" = t3 ] && units+=(fern-t3)
  for unit in "${units[@]}"; do
    on_node tee "/etc/systemd/system/$unit.service" <"deploy/systemd/$unit.service" >/dev/null
  done
  # The box user, which runs T3 and owns workspaces: systemd needs it in the
  # node's user database. A node whose uid 1000 is someone already keeps it.
  on_node sh -c 'getent group 1000 >/dev/null || groupadd --gid 1000 fern
    getent passwd 1000 >/dev/null || useradd --uid 1000 --gid 1000 --no-create-home \
      --home-dir /var/lib/fern --shell /usr/sbin/nologin fern' </dev/null
  # The daemon's arguments (set_daemon_args), none until set.
  on_node sh -c 'mkdir -p /etc/fern && [ -e /etc/fern/daemon.env ] || echo FERN_DAEMON_ARGS= >/etc/fern/daemon.env' </dev/null
  on_node systemctl daemon-reload </dev/null
  on_node systemctl enable -q "${units[@]}" </dev/null
}

# t3_installed says whether the node runs T3's server.
t3_installed() { on_node systemctl is-enabled -q fern-t3 </dev/null 2>/dev/null; }

# wait_daemon waits until the daemon answers.
wait_daemon() {
  for _ in $(seq 300); do
    fern ls >/dev/null 2>&1 </dev/null && return 0
    sleep 0.2
  done
  echo "the daemon doesn't answer: journalctl -u fern-daemon" >&2
  return 1
}

# restart_daemon restarts the daemon and waits until it answers; its boxes
# keep running.
restart_daemon() {
  on_node systemctl restart fern-daemon </dev/null
  wait_daemon
}

# wait_t3 waits until T3's server answers on its port.
wait_t3() {
  for _ in $(seq 180); do
    on_node curl -sf -o /dev/null --max-time 2 http://127.0.0.1:30773/.well-known/t3/environment </dev/null && return 0
    sleep 1
  done
  echo "T3's server doesn't answer: journalctl -u fern-t3" >&2
  return 1
}

# in_t3 runs a shell command as T3's server does: in its root, as its user,
# with its environment.
in_t3() {
  on_node bash -c 'p=$(systemctl show -p MainPID --value fern-t3) && [ "$p" != 0 ] &&
    mapfile -d "" env </proc/$p/environ &&
    exec nsenter -t "$p" -m -r -w -S 1000 -G 1000 -- env -i "${env[@]}" sh -c "$1"' in_t3 "$1"
}

# tracing_on says whether Jaeger runs on the node (lab/profile.sh on).
tracing_on() { on_node systemctl is-active -q fern-jaeger </dev/null 2>/dev/null; }

# daemon_args prints the daemon's arguments (FERN_DAEMON_ARGS in
# /etc/fern/daemon.env).
daemon_args() { on_node sh -c '. /etc/fern/daemon.env 2>/dev/null; echo "${FERN_DAEMON_ARGS:-}"' </dev/null; }

# set_daemon_args sets the daemon's arguments and restarts it.
set_daemon_args() {
  printf 'FERN_DAEMON_ARGS="%s"\n' "$*" | on_node sh -c 'mkdir -p /etc/fern && cat >/etc/fern/daemon.env'
  restart_daemon
}

# agent_boxes says whether Claude runs in a box of its own per environment
# (lab/boxes.sh agents): environment <id>'s is <id>.agent.
agent_boxes() { ! daemon_args | grep -q -- '--agent-boxes=false'; }

# box_uid prints what identifies environment $1's running box,
# runsc-<env>@<sandbox pid>, or nothing if none runs.
box_uid() {
  # runsc's IDs are hashes; its list names each box's bundle, /run/fern/boxes/<env>.
  on_node runsc --root /run/fern/runsc list </dev/null 2>/dev/null |
    awk -v b="/run/fern/boxes/$1" '$4 == b && $3 == "running" { print "runsc-" substr(b, 17) "@" $2 }'
}

# box_log prints environment $1's box's output, from the file runsc writes
# it to.
box_log() { on_node cat "/var/lib/fern/boxes/$1/log" </dev/null 2>/dev/null || true; }

# load_images unpacks images from Docker into the node's pool (fern image
# load), skipping those it has: the node keeps the Docker ID of each it
# loaded, since a VPS is far away for gigabytes.
load_images() {
  local arch image id changed=() ids=()
  case $(on_node uname -m </dev/null) in aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
  for image in "$@"; do
    id=$(docker image inspect -f '{{.Id}}' "$image")
    # fern image load names a tag's link with its slashes as underscores.
    if [ "$(on_node sh -c "test -e '/var/lib/fern/images/${image//\//_}' && cat /var/lib/fern-images/${image//[\/:]/-}" </dev/null 2>/dev/null)" != "$id" ]; then
      changed+=("$image")
      ids+=("$id")
    fi
  done
  [ ${#changed[@]} -gt 0 ] || return 0
  # Compressed on the way: a Mac's upload to a VPS is the slow part.
  docker save --platform "linux/$arch" "${changed[@]}" | gzip -1 | on_node sh -c "gunzip | $FERN_CLI image load" >/dev/null
  for i in "${!changed[@]}"; do
    echo "${ids[$i]}" | on_node sh -c "mkdir -p /var/lib/fern-images && cat >/var/lib/fern-images/${changed[$i]//[\/:]/-}"
  done
}

# tar_dir writes a tar of a directory to stdout, without the extended
# attributes and ._ files macOS's tar adds.
tar_dir() { COPYFILE_DISABLE=1 tar -C "$1" --no-xattrs -cf - .; }

# lab_lock waits until no other script uses the lab (or FERN_HOST's VPS),
# then holds it until this script and its children end: worktrees share
# the lab, the images' :dev tags and Docker, and two builds or check runs
# at once overwrite each other's images and skew each other's timings.
# The lock is the kernel's (flock, through perl: macOS has no flock
# command) on fd 9, so it ends with the last process holding fd 9, even a
# killed one. The mkdir lock before it had to take over a dead owner's
# lock, and two waiters could both do so at once.
lab_lock() {
  [ -z "${FERN_LAB_LOCKED:-}" ] || return 0
  local lock=${TMPDIR:-/tmp}/fern-lab-${FERN_HOST:-local}.flock
  exec 9>>"$lock"
  if ! _flock nb; then
    echo "waiting for the lab, held by: $(cat "$lock.owner" 2>/dev/null)" >&2
    _flock
  fi
  echo "$$ $(basename "$0") in $PWD" >"$lock.owner"
  export FERN_LAB_LOCKED=$lock
}
_flock() {
  perl -MFcntl=:flock -e 'open(my $f, ">&=", 9) or die "lab_lock: fd 9: $!\n";
    flock($f, LOCK_EX | (@ARGV ? LOCK_NB : 0)) or exit 1' "$@"
}
