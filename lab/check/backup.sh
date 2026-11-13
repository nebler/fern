# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section backup of lab/check.sh, which sources this file: it has no use by itself.
# backup runs versitygw on the node as the object store, backs a thread up to
# it, changes the thread's data, backs up again (an incremental send), and
# restores the bucket into a scratch pool: the restored workspace must be
# the backed-up one, file for file.
backup() {
  echo "== threads backed up to an object store, and restored"
  if on_node test -e /etc/fern/backup.env </dev/null; then
    note "skipped: this node has backups set up already (/etc/fern/backup.env)"
    return
  fi
  local args scratch=/tmp/fern-restore node
  args=$(daemon_args)
  node=$(on_node hostname </dev/null)
  in_daemon "podman rm -f fern-s3 >/dev/null 2>&1; podman run -d -q --name fern-s3 --network=host --tmpfs /data \
    docker.io/versity/versitygw:latest --access fernkey --secret fernsecret --port 127.0.0.1:19000 posix /data >/dev/null
    for i in \$(seq 50); do curl -s -o /dev/null http://127.0.0.1:19000/ && break; sleep 0.2; done
    curl -sf --aws-sigv4 aws:amz:us-east-1:s3 --user fernkey:fernsecret -X PUT http://127.0.0.1:19000/fern-check
    umask 077; printf 'FERN_BACKUP_ENDPOINT=http://127.0.0.1:19000\nFERN_BACKUP_REGION=us-east-1\nFERN_BACKUP_BUCKET=fern-check\nFERN_BACKUP_ACCESS_KEY=fernkey\nFERN_BACKUP_SECRET_KEY=fernsecret\nFERN_BACKUP_KEY=%s\n' \$(openssl rand -hex 32) >/etc/fern/backup.env"
  set_daemon_args $args --backup-every=24h
  fresh $ENV
  sql $ENV "insert into users (name, email) values ('Backed', 'up@example.com')" >/dev/null
  fern sleep $ENV >/dev/null
  objects() { in_daemon "curl -s --aws-sigv4 aws:amz:us-east-1:s3 --user fernkey:fernsecret 'http://127.0.0.1:19000/fern-check?list-type=2&prefix=fern/$node/envs/$ENV/'" | grep -o '<Key>' | wc -l | tr -d ' '; }
  check "fern backup backs the thread up" 0 "$(fern backup >/dev/null 2>&1; echo $?)"
  check "… as one send" 1 "$(objects)"
  sql $ENV "insert into users (name, email) values ('Again', 'again@example.com')" >/dev/null
  fern sleep $ENV >/dev/null
  check "a change, and the next backup sends it from the first" 2 "$(fern backup >/dev/null 2>&1; objects)"
  check "an unchanged thread isn't sent again" 2 "$(fern backup >/dev/null 2>&1; objects)"
  in_daemon "set -e; umount $scratch 2>/dev/null || true; rm -rf $scratch $scratch.img; mkdir -p $scratch
    truncate -s 4G $scratch.img && mkfs.btrfs -q $scratch.img && mount -o loop $scratch.img $scratch"
  check "the backup restores into another pool" 0 "$(in_daemon "fern-daemon restore $node $scratch >/dev/null 2>&1; echo \$?")"
  local snap
  snap=$(in_daemon "ls /var/lib/fern/envs/$ENV/snaps | grep '^fern-backup-' | tail -1")
  check "… the workspace file for file" "$(treehash /var/lib/fern/envs/$ENV/snaps/$snap)" "$(treehash $scratch/envs/$ENV/live)"
  check "… and its record and T3's data with it" "yes yes" \
    "$(in_daemon "for p in envs/$ENV/env.json userdata; do test -e $scratch/\$p && echo yes || echo no; done" | xargs)"
  check "a restore run again into the pool it finished is refused" 1 "$(in_daemon "fern-daemon restore $node $scratch >/dev/null 2>&1; echo \$?")"
  in_daemon "umount $scratch; rm -rf $scratch $scratch.img; rm -f /etc/fern/backup.env; podman rm -f fern-s3 >/dev/null"
  # A restore that hasn't finished keeps its marker in the pool, and then
  # neither the daemon nor T3 starts on it.
  in_daemon "printf '{\"node\":\"x\",\"state\":\"y\"}' > /var/lib/fern/restoring.json"
  on_node systemctl restart fern-daemon </dev/null >/dev/null 2>&1 || true
  sleep 2
  check "the daemon doesn't start on a half-restored pool" no "$(succeeds on_node systemctl is-active -q fern-daemon </dev/null)"
  if t3_installed; then
    on_node systemctl restart fern-t3 </dev/null >/dev/null 2>&1 || true
    check "nor does T3" no "$(succeeds on_node systemctl is-active -q fern-t3 </dev/null)"
  fi
  in_daemon "rm /var/lib/fern/restoring.json"
  restart_daemon
  if t3_installed; then on_node systemctl restart fern-t3 </dev/null; wait_t3; fi
  set_daemon_args $args
  fern rm $ENV
  # The node's other threads were backed up too: each keeps its last
  # backup snapshot, as the next backup's parent.
  local e s
  for e in $(fern ls | awk 'NR > 1 { print $1 }'); do
    for s in $(fern snapshots "$e" 2>/dev/null | awk '/^fern-backup-/ { print $1 }'); do fern rmsnap "$e" "$s" >/dev/null; done
  done
}
