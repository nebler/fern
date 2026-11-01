#!/usr/bin/env bash
# Installs Fern and T3 Code's server on a Linux server, from the images a
# release published (ghcr.io/nebler/fern*), or updates them. From your Mac:
#
#   curl -fsSL https://raw.githubusercontent.com/nebler/fern/main/install.sh | bash -s -- root@1.2.3.4
#
# It installs the server over SSH, asks once for Claude's login there
# (`claude setup-token`, your subscription), keeps an SSH tunnel to T3 and
# the previews running in the background (launchd or systemd --user) and
# copies a pairing link for the T3 app: Settings → Connections. On the
# server itself, as root, it installs without the tunnel:
#
#   curl -fsSL https://raw.githubusercontent.com/nebler/fern/main/install.sh | bash
#
# Run it again to update; every step skips what is done. Settings, in the
# environment: FERN_VERSION (a release tag, default latest), FERN_FIREWALL=off
# to keep your own firewall.
#
# The server: Ubuntu or Debian (tested: Ubuntu 26.04, Debian 13), arm64 or
# amd64, dedicated to Fern. It gets a
# firewall that lets in only SSH (T3's port must never be public), a 50 GB
# sparse btrfs pool at /var/lib/fern, gVisor and Podman.
#
# lab/vps.sh runs it with --node-only, then builds the images itself.
set -Eeuo pipefail

FERN_VERSION=${FERN_VERSION:-latest}
FERN_FIREWALL=${FERN_FIREWALL:-on}
GVISOR_RELEASE=${GVISOR_RELEASE:-20261012}
REGISTRY=ghcr.io/nebler
SCRIPT_URL=${FERN_SCRIPT_URL:-https://raw.githubusercontent.com/nebler/fern/main/install.sh}
T3_PORT=30773
PREVIEW_PORT=30780

say() { printf '\033[1m== %s\033[0m\n' "$*"; }
die() { printf 'fern install: %s\n' "$*" >&2; exit 1; }
warn() { printf 'fern install: %s\n' "$*" >&2; }
# A command that fails unexpectedly says where, so the person knows what to
# report; the steps that can fail for a known reason say why themselves.
trap 'printf "fern install: failed at line %s (%s); running it again is safe\n" "$LINENO" "$BASH_COMMAND" >&2' ERR

# ---------------------------------------------------------------- the server

# node prepares the machine: packages, the firewall, the pool, the kernel's
# settings and gVisor.
node() {
  export DEBIAN_FRONTEND=noninteractive
  { command -v mkfs.btrfs && command -v bzip2 && command -v nft && command -v nsenter && command -v podman && { command -v netavark || [ -x /usr/lib/podman/netavark ]; } && command -v curl && command -v sysctl; } >/dev/null ||
    { say "installing btrfs-progs, nftables and podman"
      # A new cloud server updates itself at its first boot, holding apt's
      # lock for a few minutes: wait for it rather than fail.
      local apt=(apt-get -o DPkg::Lock::Timeout=600)
      "${apt[@]}" update -qq && "${apt[@]}" install -yqq btrfs-progs bzip2 nftables iproute2 util-linux procps curl ca-certificates >/dev/null &&
      "${apt[@]}" install -yqq --no-install-recommends podman netavark >/dev/null; }

  # The firewall: from the public interface, only SSH may open a connection,
  # T3's port included. It loads at boot before Fern. Its table is its own:
  # the daemon replaces table inet fern, the boxes' firewall, whole at every
  # start.
  if [ "$FERN_FIREWALL" != off ]; then
    local public ssh
    public=$(ip route show default | awk '{ print $5; exit }')
    # Every port sshd listens on stays open, so a server with SSH on another
    # port doesn't lock its owner out.
    ssh=$(ss -Htlnp 2>/dev/null | awk '/"sshd"/ { sub(/.*:/, "", $4); print $4 }' | sort -un | paste -sd, -)
    ssh=${ssh:-22}
    mkdir -p /etc/fern
    cat >/etc/fern/firewall.nft <<NFT
table inet fern_host
delete table inet fern_host
table inet fern_host {
  chain in {
    type filter hook prerouting priority -150; policy accept;
    iifname "$public" ct state new tcp dport != { $ssh } drop
    iifname "$public" ct state new udp dport != 68 drop
  }
}
NFT
    cat >/etc/systemd/system/fern-firewall.service <<'UNIT'
[Unit]
Description=Fern's firewall: only SSH from outside
# Like Ubuntu's nftables.service: no default dependencies, which would wait
# for sysinit.target, which waits for cloud-init's network stage, which
# waits for the network this unit holds back. The boot hung there.
DefaultDependencies=no
Before=fern-daemon.service fern-t3.service network-pre.target shutdown.target
Wants=network-pre.target
Conflicts=shutdown.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/fern/firewall.nft
[Install]
WantedBy=sysinit.target
UNIT
    systemctl daemon-reload
    # A node set up before 2026-10-31 has it in multi-user.target.
    systemctl disable -q fern-firewall
    systemctl enable -q fern-firewall
    systemctl restart fern-firewall
  fi

  # The pool: a sparse image file on the server's own disk, which is faster
  # than a network volume. fstab mounts it at boot; Fern's units wait for it.
  if ! mountpoint -q /var/lib/fern; then
    mkdir -p /var/lib/fern-img /var/lib/fern
    if [ ! -f /var/lib/fern-img/pool.img ]; then
      truncate -s 50G /var/lib/fern-img/pool.img
      mkfs.btrfs -q /var/lib/fern-img/pool.img
    fi
    grep -q ' /var/lib/fern ' /etc/fstab ||
      echo '/var/lib/fern-img/pool.img /var/lib/fern btrfs loop,noatime,nofail 0 0' >>/etc/fstab
    mount /var/lib/fern
    echo "mounted the pool"
  fi
  # A pool that doesn't mount at boot must not stop the boot (emergency mode,
  # without SSH): Fern's units wait for it instead (RequiresMountsFor).
  sed -i -E 's#^(/var/lib/fern-img/pool.img /var/lib/fern btrfs )loop,noatime( |$)#\1loop,noatime,nofail\2#' /etc/fstab

  # gVisor mirrors all of a box's memory mappings into one host process; at
  # the kernel's default limit, large builds (Node, JVM) segfault. Boxes reach
  # the internet through the node, which forwards for them.
  mkdir -p /etc/sysctl.d
  printf 'vm.max_map_count=4194304\nnet.ipv4.ip_forward=1\n' >/etc/sysctl.d/90-fern.conf
  sysctl -q --system

  if ! { /usr/local/bin/runsc --version 2>/dev/null | grep -q "release-$GVISOR_RELEASE" && [ -x /usr/local/bin/gvisor-bin/gvisor_sentry ]; }; then
    local url tmp
    url=https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE/$(uname -m)
    tmp=$(mktemp -d)
    curl -sSfL -o "$tmp/gvisor.tar.bz2" "$url/gvisor.tar.bz2"
    curl -sSfL -o "$tmp/gvisor.tar.bz2.sha512" "$url/gvisor.tar.bz2.sha512"
    (cd "$tmp" && sha512sum -c --quiet gvisor.tar.bz2.sha512)
    tar -xjf "$tmp/gvisor.tar.bz2" -C /usr/local/bin
    rm -rf "$tmp"
    echo "installed gVisor $GVISOR_RELEASE"
  fi
}

# fern_from_release installs the release's binaries, units and images, and
# (re)starts the services when any of them changed.
fern_from_release() {
  local name changed=no
  say "fetching Fern $FERN_VERSION (about 1 GB the first time)"
  for name in fern fern-box fern-t3; do
    podman pull -q "$REGISTRY/$name:$FERN_VERSION" >/dev/null ||
      die "can't download $REGISTRY/$name:$FERN_VERSION: is FERN_VERSION a release (https://github.com/nebler/fern/releases), and does the server reach ghcr.io?"
  done

  # The binaries, units and sample project are in the fern image.
  local cid tmp bin
  tmp=$(mktemp -d)
  cid=$(podman create "$REGISTRY/fern:$FERN_VERSION")
  podman cp "$cid:/usr/local/bin/fern" "$tmp/fern"
  podman cp "$cid:/usr/local/bin/fern-daemon" "$tmp/fern-daemon"
  podman cp "$cid:/usr/share/fern" "$tmp/share"
  podman rm "$cid" >/dev/null
  for bin in fern fern-daemon; do
    if ! cmp -s "$tmp/$bin" "/usr/local/bin/$bin"; then
      install -m 755 "$tmp/$bin" "/usr/local/bin/$bin.new" && mv "/usr/local/bin/$bin.new" "/usr/local/bin/$bin"
      changed=yes
    fi
  done
  for unit in fern-daemon fern-t3; do
    if ! cmp -s "$tmp/share/systemd/$unit.service" "/etc/systemd/system/$unit.service"; then
      install -m 644 "$tmp/share/systemd/$unit.service" "/etc/systemd/system/$unit.service"
      changed=yes
    fi
  done
  rm -rf /usr/share/fern && mv "$tmp/share" /usr/share/fern
  rm -rf "$tmp"

  # The box user, which runs T3 and owns workspaces: systemd needs it in the
  # node's user database. A node whose uid 1000 is someone already keeps it.
  getent group 1000 >/dev/null || groupadd --gid 1000 fern
  getent passwd 1000 >/dev/null || useradd --uid 1000 --gid 1000 --no-create-home \
    --home-dir /var/lib/fern --shell /usr/sbin/nologin fern
  mkdir -p /etc/fern
  [ -e /etc/fern/daemon.env ] || echo FERN_DAEMON_ARGS= >/etc/fern/daemon.env

  # The daemon and T3's unit name the images fern-box:dev and fern-t3:dev.
  # Each is unpacked into the pool once per image ID, as lab/build.sh does.
  local load=() id
  mkdir -p /var/lib/fern-images
  for name in fern-box fern-t3; do
    id=$(podman image inspect -f '{{.Id}}' "$REGISTRY/$name:$FERN_VERSION")
    podman tag "$REGISTRY/$name:$FERN_VERSION" "$name:dev"
    if [ ! -e "/var/lib/fern/images/$name:dev" ] || [ "$(cat "/var/lib/fern-images/$name-dev" 2>/dev/null)" != "$id" ]; then
      load+=("$name:dev")
    fi
  done
  if [ ${#load[@]} -gt 0 ]; then
    say "unpacking ${load[*]}"
    podman save -m "${load[@]}" | /usr/local/bin/fern image load >/dev/null
    for name in "${load[@]}"; do
      podman image inspect -f '{{.Id}}' "$name" >"/var/lib/fern-images/${name/:/-}"
    done
    changed=yes
  fi

  systemctl daemon-reload
  systemctl enable -q fern-daemon fern-t3
  # A run that failed may have left them past systemd's start limit.
  systemctl reset-failed fern-daemon fern-t3 2>/dev/null || true
  if [ "$changed" = yes ]; then
    systemctl restart fern-daemon
    wait_daemon
    systemctl restart fern-t3
  else
    systemctl start fern-daemon fern-t3
  fi
  wait_t3
  echo "Fern $FERN_VERSION is running"
}

wait_daemon() {
  for _ in $(seq 150); do
    /usr/local/bin/fern ls >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  die "the daemon doesn't answer: journalctl -u fern-daemon"
}

wait_t3() {
  for _ in $(seq 180); do
    curl -sf -o /dev/null --max-time 2 "http://127.0.0.1:$T3_PORT/.well-known/t3/environment" && return 0
    sleep 1
  done
  die "T3's server doesn't answer: journalctl -u fern-t3"
}

# in_t3 runs a shell command as T3's server does: in its root, as its user,
# with its environment.
in_t3() {
  local pid
  pid=$(systemctl show -p MainPID --value fern-t3)
  [ "$pid" != 0 ] || die "T3's server isn't running"
  local env=()
  mapfile -d '' env <"/proc/$pid/environ"
  nsenter -t "$pid" -m -r -w -S 1000 -G 1000 -- env -i "${env[@]}" sh -c "$1"
}

t3() { in_t3 "node /opt/t3/apps/server/dist/bin.mjs $(printf '%q ' "$@")"; }

# login stores Claude's login once: the token `claude setup-token` prints,
# read without echoing, in /etc/fern/claude.env, which only root reads.
login() {
  [ -s /etc/fern/claude.env ] && return
  [ -r /dev/tty ] || die "Claude's login needs a terminal: run this again in one, or store the token from 'claude setup-token' with lab/claude-login.sh"
  say "Claude's login (your subscription, not an API key)"
  echo "Claude Code prints a link: open it, sign in, paste the code back here; it then prints a token."
  podman run --rm -it --network=host --entrypoint /usr/local/libexec/claude \
    "$REGISTRY/fern-t3:$FERN_VERSION" setup-token </dev/tty >/dev/tty || true
  local token
  read -rsp "Paste the token it printed (starts with sk-ant-oat; it won't show): " token </dev/tty
  echo
  case "$token" in
    sk-ant-oat*) ;;
    *) die "that isn't a token from 'claude setup-token'; run this again" ;;
  esac
  (umask 077 && printf 'CLAUDE_CODE_OAUTH_TOKEN=%s\n' "$token" >/etc/fern/claude.env.new &&
    mv /etc/fern/claude.env.new /etc/fern/claude.env)
  unset token
  echo "stored the login in /etc/fern/claude.env"
  # T3 reads it when it starts; the daemon at every wake.
  systemctl restart fern-t3
  wait_t3
}

# sample adds the sample project (Postgres with three users) to T3 once, so
# a first thread can try a revert that brings the database back.
sample() {
  local r=/var/lib/fern/repos/sample
  [ -d "$r/.git" ] && return
  install -d -o 1000 -g 1000 "$r"
  cp -r /usr/share/fern/sample-repo/. "$r/"
  chown -R 1000:1000 "$r"
  in_t3 "cd $r && git init -q -b main && git add -A &&
    git -c user.email=fern@localhost -c user.name=fern commit -qm 'Sample repo'"
  t3 project add --title sample "$r" >/dev/null
  echo "added the sample project"
}

# pair prints a pairing link for T3 at 127.0.0.1:$T3_PORT, the tunnel's end,
# that works for 15 minutes.
pair() {
  t3 pair --ttl 15m --label "fern on $(hostname -s)" |
    sed -nE "s#^Pairing URL: *http://[^/]+/#http://127.0.0.1:$T3_PORT/#p"
}

server() {
  [ "$(id -u)" = 0 ] || die "run it as root"
  [ "$(uname -s)" = Linux ] && command -v apt-get >/dev/null && command -v systemctl >/dev/null ||
    die "the server must be Ubuntu or Debian with systemd"
  case $(uname -m) in aarch64 | x86_64) ;; *) die "the server must be arm64 or amd64" ;; esac
  local mem disk
  mem=$(awk '/^MemTotal:/ { print int($2 / 1024) }' /proc/meminfo)
  [ "$mem" -ge 3500 ] || warn "the server has $mem MB of memory; Fern and T3 want 4 GB or more"
  disk=$(df -Pm / | awk 'NR == 2 { print $4 }')
  [ "$disk" -ge 10240 ] || warn "the server has $((disk / 1024)) GB of disk free; Fern wants 10 GB or more"
  node
  [ "${1:-}" = --node-only ] && return
  fern_from_release
  login
  sample
  # The Mac's half of this script finds the link in this line.
  echo "pairing link: $(pair)"
}

# ---------------------------------------------------------------- your Mac

# tunnel keeps an SSH tunnel to T3 and the previews running in the
# background, and after a reboot.
tunnel() {
  local host=$1 args
  args=(ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3
    -L "127.0.0.1:$T3_PORT:127.0.0.1:$T3_PORT" -L "127.0.0.1:$PREVIEW_PORT:127.0.0.1:$PREVIEW_PORT" "$host")
  case $(uname -s) in
    Darwin)
      local label=dev.fern.tunnel plist=$HOME/Library/LaunchAgents/dev.fern.tunnel.plist
      mkdir -p "$HOME/Library/LaunchAgents"
      {
        printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n'
        printf '<plist version="1.0"><dict>\n<key>Label</key><string>%s</string>\n<key>ProgramArguments</key><array>\n' "$label"
        printf '<string>%s</string>\n' /usr/bin/ssh "${args[@]:1}"
        printf '</array>\n<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><true/>\n<key>ThrottleInterval</key><integer>10</integer>\n'
        printf '<key>StandardErrorPath</key><string>%s</string>\n</dict></plist>\n' "$HOME/Library/Logs/fern-tunnel.log"
      } >"$plist"
      launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
      launchctl bootstrap "gui/$(id -u)" "$plist"
      echo "the tunnel runs in the background (launchd: $label; log ~/Library/Logs/fern-tunnel.log)"
      ;;
    Linux)
      local unit=$HOME/.config/systemd/user/fern-tunnel.service
      mkdir -p "$(dirname "$unit")"
      printf '[Unit]\nDescription=SSH tunnel to Fern\n[Service]\nExecStart=%s\nRestart=always\nRestartSec=10\n[Install]\nWantedBy=default.target\n' \
        "$(command -v ssh) $(printf '%q ' "${args[@]:1}")" >"$unit"
      systemctl --user daemon-reload
      systemctl --user enable -q fern-tunnel
      systemctl --user restart fern-tunnel
      echo "the tunnel runs in the background (systemctl --user status fern-tunnel)"
      ;;
    *)
      echo "keep this running while you use Fern: ${args[*]}"
      ;;
  esac
}

remote() {
  local host=$1 run
  command -v ssh >/dev/null || die "needs ssh"
  # The first connection to a new server learns its host key (accept-new).
  ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new "$host" true ||
    die "can't log in to $host with an SSH key. Check 'ssh $host' works without a password (ssh-copy-id $host sets that up)"
  # Run this same script there: from its file, or fetched here when it came
  # through a pipe, so the server needs nothing but SSH.
  if [ -f "${BASH_SOURCE[0]:-}" ]; then
    ssh -o BatchMode=yes "$host" 'cat >/tmp/fern-install.sh' <"${BASH_SOURCE[0]}"
  else
    curl -fsSL "$SCRIPT_URL" | ssh -o BatchMode=yes "$host" 'cat >/tmp/fern-install.sh'
  fi
  run="FERN_VERSION=$(printf %q "$FERN_VERSION") FERN_FIREWALL=$(printf %q "$FERN_FIREWALL") bash /tmp/fern-install.sh server"
  # One session for all of it, with a terminal for Claude's login and sudo's
  # password; its output is kept for the pairing link.
  local out
  out=$(mktemp)
  ssh -t "$host" "if [ \"\$(id -u)\" = 0 ]; then $run; else sudo $run; fi" | tee "$out"
  local link
  link=$(tr -d '\r' <"$out" | sed -n 's/^pairing link: //p' | tail -1)
  rm -f "$out"
  [ -n "$link" ] || die "the server gave no pairing link; see the messages above"

  say "connecting"
  tunnel "$host"
  # The tunnel runs in the background: check it reaches T3 before saying so.
  local ok=no
  for _ in $(seq 20); do
    if curl -sf -o /dev/null --max-time 2 "http://127.0.0.1:$T3_PORT/.well-known/t3/environment"; then ok=yes && break; fi
    sleep 1
  done
  if [ "$ok" = no ]; then
    warn "the tunnel doesn't reach T3 on 127.0.0.1:$T3_PORT. Is the port taken (lsof -i :$T3_PORT), or does your SSH key need a passphrase that isn't in your keychain (ssh-add --apple-use-keychain)?"
    [ -f "$HOME/Library/Logs/fern-tunnel.log" ] && tail -3 "$HOME/Library/Logs/fern-tunnel.log" >&2
    exit 1
  fi

  local copied=""
  if command -v pbcopy >/dev/null; then printf %s "$link" | pbcopy && copied=" (copied)"; fi
  cat <<EOF

Fern is ready. In the T3 app: Settings → Connections → add this link$copied,
which works for 15 minutes (run this again for a new one):

  $link

Then open the sample project and start a thread in New worktree. Previews:
http://<thread>.localhost:$PREVIEW_PORT/
EOF
}

case "${1:-}" in
  pair) pair ;;
  server) server ;;
  --node-only) server --node-only ;;
  "") server && say "done; T3 listens on 127.0.0.1:$T3_PORT only through an SSH tunnel: from your Mac, run this with root@<this server> for one" ;;
  -*) die "unknown option $1" ;;
  *) remote "$1" ;;
esac
