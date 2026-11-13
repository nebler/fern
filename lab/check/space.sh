# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section space of lab/check.sh, which sources this file: it has no use by itself.
# space checks fern df and fern gc on a node with an image a tag has moved
# off, which nothing runs from.
space() {
  echo "== what uses the disk, and what nothing needs"
  local old
  old=$(printf 'e%.0s' $(seq 64))
  fresh $ENV
  fern exec $ENV -- sh -c 'head -c 3000000 /dev/urandom > blob'
  fern snapshot $ENV s1 >/dev/null
  in_daemon "mkdir -p /var/lib/fern/images/$old/root && head -c 1000000 /dev/urandom > /var/lib/fern/images/$old/root/f"
  fern df >"$TMP/df"
  check "df shows the pool" yes "$(succeeds grep -q '^the pool .*/var/lib/fern' "$TMP/df")"
  check "df shows the server's disk, which the pool is a file on" yes "$(succeeds grep -q "^the server's disk .*/var/lib/fern-img" "$TMP/df")"
  check "df lists the environment" yes "$(succeeds grep -q "^environment $ENV " "$TMP/df")"
  check "df says gc removes an image nothing runs from" yes "$(succeeds grep -q "^image ${old:0:12} .*fern gc removes it" "$TMP/df")"
  if in_daemon "command -v podman" >/dev/null 2>&1; then
    note "fern gc skipped: on this node it would remove Podman's images, the next build's cache"
    in_daemon "rm -rf /var/lib/fern/images/$old"
    return
  fi
  fern gc >"$TMP/gc"
  check "gc removes the image nothing runs from" no "$(succeeds in_daemon "test -e /var/lib/fern/images/$old")"
  check "gc says what it removed" yes "$(succeeds grep -q "^removed image ${old:0:12}" "$TMP/gc")"
  check "gc keeps the box image" yes "$(succeeds in_daemon "test -e /var/lib/fern/images/fern-box:dev/root")"
  check "the environment still wakes" hi "$(fern exec $ENV -- echo hi)"
  # A turn's snapshot that fails doesn't fail the turn (T3 goes on): it is
  # recorded with the environment, listed by fern doctor, and told to the
  # thread's Claude, until a snapshot works again.
  # A tmpfs over its snapshots' folder: btrfs can't snapshot onto it.
  in_daemon "mount -t tmpfs -o size=1m fern-check /var/lib/fern/envs/$ENV/snaps"
  fern snapshot $ENV t3-lost >/dev/null 2>&1 || true
  check "a failed snapshot is recorded with its environment" yes "$(succeeds in_daemon "grep -q t3-lost /var/lib/fern/envs/$ENV/snapshot-failed.json")"
  check "fern doctor lists it" 1 "$(fern doctor 2>/dev/null | grep -c "^FAIL  snapshot .*$ENV: the snapshot of a turn wasn't taken (t3-lost)" || true)"
  if [ "${AGENTS:-0}" = 1 ]; then
    fern sleep $ENV >/dev/null
    check "the thread's Claude is told which checkpoint has none" yes \
      "$(fern exec --agent $ENV -- cat /var/lib/fern/envs/$ENV/claude/CLAUDE.md | tr '\n' ' ' | grep -q 'checkpoint t3-lost' && echo yes || echo no)"
  fi
  in_daemon "umount /var/lib/fern/envs/$ENV/snaps"
  fern snapshot $ENV t3-next >/dev/null
  check "the next snapshot that works clears it" no "$(succeeds in_daemon "test -e /var/lib/fern/envs/$ENV/snapshot-failed.json")"
  # fern pool grow: the pool's file, its loop device and btrfs, as was done
  # by hand on the VPS; run again with the size it has, it changes nothing.
  local before after again
  before=$(in_daemon "stat -c %s /var/lib/fern-img/pool.img")
  fern pool grow +1G >/dev/null
  after=$(in_daemon "stat -c %s /var/lib/fern-img/pool.img")
  check "fern pool grow +1G makes the pool's file 1 GiB bigger" $((1 << 30)) $((after - before))
  check "and btrfs takes all of it" "$after" "$(in_daemon "btrfs filesystem show --raw /var/lib/fern | awk '/devid/ { print \$4 }'")"
  fern pool grow "$((after >> 30))G" >/dev/null
  again=$(in_daemon "stat -c %s /var/lib/fern-img/pool.img")
  check "run again with the size it has, it changes nothing" "$after" "$again"
}
