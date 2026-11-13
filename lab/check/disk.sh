# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section disk of lab/check.sh, which sources this file: it has no use by itself.
disk() {
  echo "== a big workspace"
  local D=/var/lib/fern/envs/big
  local -a snapshot=(fern snapshot) restore_disk=(fern restore)
  # Resource reads alter the I/O cadence before seeded-create. Profile
  # these large-workspace operations only in all or explicit CPU runs.
  if [ "${DISK_HARDWARE:-0}" = 1 ]; then
    snapshot=(resource_op disk.snapshot snapshot)
    restore_disk=(resource_op disk.restore restore)
  fi
  fern rm big 2>/dev/null || true
  fern create big >/dev/null
  # 1,000 folders of 100 small files, as node_modules has, and a 1 GB file.
  in_daemon "cd $D/live && mkdir -p node_modules/p0 && cd node_modules &&
    for f in \$(seq 100); do head -c 1024 /dev/urandom > p0/f\$f; done &&
    for d in \$(seq 999); do cp -r --reflink=never p0 p\$d; done &&
    head -c 1000000000 /dev/urandom > ../blob && chown -R 1000:1000 $D/live && sync"
  note "workspace: $(in_daemon "find $D/live | wc -l") files, $(in_daemon "du -sh $D/live | cut -f1")"
  for i in $(seq 10); do "${snapshot[@]}" big "s$i" | took; done >"$TMP/snap"
  metric disk.snapshot_ms "snapshot, ms" "$(stats <"$TMP/snap")"
  in_daemon "echo changed > $D/live/blob && rm $D/live/node_modules/p0/f1 && echo new > $D/live/new"
  for i in $(seq 5); do "${restore_disk[@]}" big "s$i" | took; done >"$TMP/restore"
  metric disk.restore_ms "restore (asleep, so no box to stop), ms" "$(stats <"$TMP/restore")"
  check "a restore equals its snapshot (tree hash)" "$(treehash $D/snaps/s1)" "$(treehash $D/live)"
  start=$(now_ms)
  fern rm big
  metric disk.rm_ms "rm with 10 snapshots, ms" $(($(now_ms) - start))
  check "rm leaves nothing behind" no "$(succeeds in_daemon "test -e $D")"

  python3 -c '
import os, sys
for d in range(200):
    os.makedirs(f"{sys.argv[1]}/d{d}")
    for f in range(100):
        open(f"{sys.argv[1]}/d{d}/f{f}", "wb").write(os.urandom(1024))' "$TMP/repo"
  start=$(now_ms)
  fern create seeded --repo "$TMP/repo" >/dev/null
  metric disk.create_seeded_ms "create seeded from the Mac with 20,000 files (20 MB), ms" $(($(now_ms) - start))
  fern rm seeded

  fresh $ENV
  start=$(now_ms)
  if fern exec $ENV -- git clone -q --depth 1 https://github.com/pingdotgg/t3code.git t3code; then
    note "git clone --depth 1 of t3code in a box: $(($(now_ms) - start)) ms," \
      "$(fern exec $ENV -- sh -c 'find t3code | wc -l') files, $(fern exec $ENV -- du -sh t3code | cut -f1)"
    note "snapshot with it: $(fern snapshot $ENV c1 | took) ms"
  else
    note "git clone of t3code failed"
  fi
}
