#!/usr/bin/env bash
# Spike: a disk limit per environment with btrfs simple quotas. Does the
# accounting say what an environment wrote and still holds, does a limit stop
# a write, and what do snapshots, restores and deletes cost with it on?
# Runs in a privileged golang container with a loop-mounted btrfs, as
# lab/test-store.sh does: not in the lab's node, and never on FERN_HOST.
#   lab/spike/squota/run.sh [versions] [accounting] [groups] [limit] [existing] [bench] [big]   (default: all seven)
# bench and big are the timings (90 minutes with ROUNDS=8, the default): they hold
# the lab lock, so no other lab script runs meanwhile (a worktree's go test
# still can). It prints what it measures; RESULTS.md has the run that was
# kept.
set -euo pipefail

if [ "${1:-}" != --inside ]; then
  source "$(dirname "$0")/../../lib.sh"
  [ -z "$FERN_HOST" ] || { echo "this spike runs in the Mac's Docker, not on FERN_HOST" >&2; exit 1; }
  case " ${*:-bench big} " in *" bench "* | *" big "*) lab_lock ;; esac
  "${GO_RUN[@]}" --privileged -e DEBIAN_FRONTEND=noninteractive \
    -e ROUNDS="${ROUNDS:-8}" -e LIGHT="${LIGHT:-100}" -e HEAVY="${HEAVY:-5}" -e BIG="${BIG:-30}" \
    golang:1.27 bash lab/spike/squota/run.sh --inside "$@"
  exit
fi
shift
[ $# -gt 0 ] || set -- versions accounting groups limit existing bench big

P=/mnt/pool
M=$((1 << 20))
say() { printf '\n== %s\n' "$*"; }

# fresh makes a new filesystem at $P, mounted as install.sh mounts the pool.
fresh() {
  cd /
  umount $P 2>/dev/null || true
  rm -f /tmp/pool.img
  truncate -s 8G /tmp/pool.img
  mkfs.btrfs -q /tmp/pool.img
  mkdir -p $P
  mount -o loop,noatime /tmp/pool.img $P
}
id() { btrfs inspect-internal rootid "$1"; }
# fill writes files f<first> … of 1 MiB each into a folder; a file written
# again gets new blocks, and its old ones are free unless a snapshot holds them.
fill() {
  local i
  for ((i = $2; i < $2 + $3; i++)); do dd if=/dev/urandom of="$1/f$i" bs=1M count=1 status=none; done
}
# settle commits what was written and waits for deleted subvolumes to be
# cleaned: simple quotas count at the commit, and a deleted subvolume's
# blocks are freed by btrfs's cleaner afterwards.
settle() {
  btrfs filesystem sync $P
  btrfs subvolume sync $P >/dev/null
  btrfs filesystem sync $P
}
# show prints every group's usage in bytes and MiB after a step.
show() {
  settle
  printf -- '-- %s\n' "$*"
  btrfs qgroup show --raw -p $P | awk 'NR > 2 { path = $5; for (i = 6; i <= NF; i++) path = path " " $i
    sub(/<0 member qgroups>/, "", path)
    printf "   %-7s %12d  %8.2f MiB  in %-6s %s\n", $1, $2, $2 / 1048576, $4, path }'
}
# join puts a subvolume into a group after it was made.
join() { btrfs qgroup assign "0/$(id "$1")" "$2" $P; }
# as_box runs a command as the box's user, who owns the workspaces.
as_box() { setpriv --reuid 1000 --regid 1000 --clear-groups "$@"; }

versions() {
  say "versions"
  grep PRETTY_NAME /etc/os-release
  echo "kernel $(uname -r), $(uname -m)"
  btrfs --version | head -1
  go version
  fresh
  btrfs quota enable --simple $P && echo "btrfs quota enable --simple: ok"
  local uuid
  uuid=$(btrfs filesystem show $P | sed -n 's/.*uuid: //p')
  echo "/sys/fs/btrfs/<uuid>/qgroups/mode: $(cat "/sys/fs/btrfs/$uuid/qgroups/mode")"
  btrfs qgroup create 1/1 $P
  echo "a group's files in sysfs: $(ls "/sys/fs/btrfs/$uuid/qgroups/1_1" | tr '\n' ' ')"
}

accounting() {
  say "accounting: one environment (group 1/1), every subvolume put in after it is made"
  fresh
  btrfs quota enable --simple $P
  mkdir -p $P/envs/e1/snaps $P/envs/e2/snaps $P/envs/builder/snaps $P/envs/e1-fork-1 $P/worktrees
  btrfs qgroup create 1/1 $P
  btrfs subvolume create $P/envs/e1/live >/dev/null
  join $P/envs/e1/live 1/1
  show "e1/live made (subvolume $(id $P/envs/e1/live)), empty"
  fill $P/envs/e1/live 0 100
  show "e1 wrote 100 files of 1 MiB"
  btrfs subvolume snapshot -r $P/envs/e1/live $P/envs/e1/snaps/s1 >/dev/null
  join $P/envs/e1/snaps/s1 1/1
  show "snapshot s1 ($(id $P/envs/e1/snaps/s1))"
  fill $P/envs/e1/live 0 50
  show "e1 wrote 50 of the files again: s1 holds the old 50 MiB"
  btrfs subvolume delete $P/envs/e1/snaps/s1 >/dev/null
  show "s1 deleted"
  btrfs qgroup clear-stale $P >/dev/null
  show "btrfs qgroup clear-stale"

  say "accounting: a golden in the builder's group (1/2), a thread started from it (1/3)"
  btrfs qgroup create 1/2 $P
  btrfs qgroup create 1/3 $P
  btrfs subvolume create $P/envs/builder/live >/dev/null
  join $P/envs/builder/live 1/2
  fill $P/envs/builder/live 1000 200
  btrfs subvolume snapshot -r $P/envs/builder/live $P/envs/builder/snaps/golden >/dev/null
  join $P/envs/builder/snaps/golden 1/2
  show "the builder wrote 200 MiB; snapshot golden"
  btrfs subvolume snapshot $P/envs/builder/snaps/golden $P/envs/e2/live >/dev/null
  join $P/envs/e2/live 1/3
  show "e2/live ($(id $P/envs/e2/live)) is a writable snapshot of golden, in 1/3"
  fill $P/envs/e2/live 0 10
  show "e2 wrote 10 MiB of its own"
  fill $P/envs/e2/live 1000 20
  show "e2 wrote 20 of the golden's files again"
  btrfs subvolume delete $P/envs/builder/snaps/golden $P/envs/builder/live >/dev/null
  show "the golden and the builder's workspace deleted (pruned, rebuilt): e2 still holds 180 MiB of it"
  btrfs qgroup clear-stale $P 2>&1 | sed 's/^/   clear-stale: /'
  show "btrfs qgroup clear-stale: a deleted subvolume's group stays while its blocks do"

  say "accounting: a checkout reflinked in from a plain folder, as an adopt does"
  mkdir $P/worktrees/plain
  fill $P/worktrees/plain 0 30
  settle
  cp -a --reflink=always $P/worktrees/plain/. $P/envs/e2/live/checkout
  show "30 MiB written in a plain folder of the pool, reflinked into e2/live"
  rm -rf $P/worktrees/plain
  show "the plain folder removed: the pool's top level (0/5) is still charged"

  say "accounting: a restore, as store.Restore does it"
  btrfs subvolume snapshot -r $P/envs/e1/live $P/envs/e1/snaps/s2 >/dev/null
  join $P/envs/e1/snaps/s2 1/1
  fill $P/envs/e1/live 100 30
  show "snapshot s2, then e1 wrote 30 MiB more"
  local old
  old=$(id $P/envs/e1/live)
  btrfs subvolume snapshot $P/envs/e1/snaps/s2 $P/envs/e1/live.new >/dev/null
  join $P/envs/e1/live.new 1/1
  btrfs subvolume snapshot -r $P/envs/e1/live $P/envs/e1/snaps/pre-restore >/dev/null
  join $P/envs/e1/snaps/pre-restore 1/1
  mv --exchange --no-target-directory $P/envs/e1/live.new $P/envs/e1/live
  btrfs subvolume delete $P/envs/e1/live.new >/dev/null
  show "restored to s2: live is subvolume $(id $P/envs/e1/live); the old one ($old) is deleted, and s2 and pre-restore hold its blocks"
  btrfs subvolume delete $P/envs/e1/snaps/pre-restore >/dev/null
  show "pre-restore deleted (a day later): the 30 MiB are free"
  fill $P/envs/e1/live 0 10
  show "e1 wrote 10 of the files again in the restored workspace"

  say "accounting: a fork in the thread's own group, and one in a group of its own (1/4)"
  btrfs qgroup create 1/4 $P
  btrfs subvolume snapshot $P/envs/e1/live $P/envs/e1-fork-1/live >/dev/null
  join $P/envs/e1-fork-1/live 1/4
  fill $P/envs/e1-fork-1/live 200 5
  show "e1-fork-1 ($(id $P/envs/e1-fork-1/live)) in 1/4 wrote 5 MiB"
  btrfs qgroup remove "0/$(id $P/envs/e1-fork-1/live)" 1/4 $P
  join $P/envs/e1-fork-1/live 1/1
  show "the same fork moved into e1's group, 1/1"

  say "accounting: delete e1 and its fork, and their group"
  btrfs subvolume delete $P/envs/e1-fork-1/live $P/envs/e1/live $P/envs/e1/snaps/s2 >/dev/null
  show "every subvolume of e1 deleted"
  btrfs qgroup destroy 1/1 $P 2>&1 | sed 's/^/   destroy 1\/1 with its stale members: /' || true
  btrfs qgroup clear-stale $P >/dev/null
  btrfs qgroup destroy 1/1 $P 2>&1 | sed 's/^/   destroy 1\/1 after clear-stale: /' || true
  show "after clear-stale and destroy 1/1"
  btrfs filesystem sync $P
  btrfs inspect-internal dump-tree -t quota /tmp/pool.img | grep -A3 'QGROUP_INFO 1/1)' | sed 's/^/   /'
  btrfs qgroup create 1/9 $P
  btrfs qgroup destroy 1/9 $P && echo "   a group that never had a member: destroyed"

  say "accounting: three ways into a group, 10 snapshots each, then all 10 deleted"
  fresh
  btrfs quota enable --simple $P
  local way i
  for way in inherit assign contained; do
    case $way in
    inherit) btrfs qgroup create 1/1 $P && btrfs subvolume create -i 1/1 $P/inherit >/dev/null && mkdir $P/inherit.snaps ;;
    assign) btrfs qgroup create 1/2 $P && btrfs subvolume create $P/assign >/dev/null && join $P/assign 1/2 && mkdir $P/assign.snaps ;;
    # The environment's folder is itself a subvolume in the group: what is
    # made inside it joins its groups (the kernel's rule for simple quotas).
    contained) btrfs qgroup create 1/3 $P && btrfs subvolume create -i 1/3 $P/outer >/dev/null &&
      btrfs subvolume create $P/outer/contained >/dev/null && mkdir $P/outer/contained.snaps ;;
    esac
  done
  fill $P/inherit 0 5
  fill $P/assign 0 5
  fill $P/outer/contained 0 5
  for i in $(seq 10); do
    btrfs subvolume snapshot -r -i 1/1 $P/inherit $P/inherit.snaps/$i >/dev/null
    btrfs subvolume snapshot -r $P/assign $P/assign.snaps/$i >/dev/null
    join $P/assign.snaps/$i 1/2
    btrfs subvolume snapshot -r $P/outer/contained $P/outer/contained.snaps/$i >/dev/null
  done
  settle
  btrfs qgroup show --raw -p $P | awk 'NR > 2 && $5 !~ /snaps/ { printf "   %-7s %12d  in %-6s %s\n", $1, $2, $4, $1 ~ /^1/ ? "" : $5 }'
  echo "   (1/1: snapshot -i; 1/2: snapshot, then qgroup assign; 1/3: made inside a subvolume that is in the group)"
  for i in $(seq 10); do
    btrfs subvolume delete $P/inherit.snaps/$i $P/assign.snaps/$i $P/outer/contained.snaps/$i >/dev/null
  done
  settle
  btrfs qgroup clear-stale $P >/dev/null
  show "the 30 snapshots deleted: a group should say what its one member does (and 1/3 its two)"
}

# groups: what a group says once its members are gone, and whether it can
# be destroyed then, for each way into it.
groups() {
  say "groups: a member's snapshots made with -i, put in with assign, and a member written to before it joined"
  fresh
  btrfs quota enable --simple $P
  local g i
  for g in 1 2 3 4; do btrfs qgroup create 1/$g $P; done
  ones() { settle; printf -- '-- %s\n' "$*"; btrfs qgroup show --raw -p $P | awk 'NR > 2 && $1 ~ /^1/ { printf "   %-5s %10d\n", $1, $2 }'; }
  btrfs subvolume create -i 1/1 $P/a >/dev/null
  fill $P/a 0 5
  for i in 1 2 3; do btrfs subvolume snapshot -r -i 1/1 $P/a $P/a.$i >/dev/null; done
  ones "1/1: a wrote 5 MiB, 3 snapshots, all made with -i 1/1"
  btrfs subvolume delete $P/a.1 $P/a.2 $P/a.3 >/dev/null
  ones "1/1: the 3 snapshots deleted"
  btrfs subvolume delete $P/a >/dev/null
  ones "1/1: a deleted too: what is left is 16 KiB per snapshot"
  btrfs subvolume create $P/b >/dev/null
  join $P/b 1/2
  fill $P/b 0 5
  for i in 1 2 3; do btrfs subvolume snapshot -r $P/b $P/b.$i >/dev/null && join $P/b.$i 1/2; done
  ones "1/2: b wrote 5 MiB, 3 snapshots, each put in after it was made"
  btrfs subvolume delete $P/b.1 $P/b.2 $P/b.3 $P/b >/dev/null
  ones "1/2: all 4 deleted"
  btrfs subvolume create $P/c >/dev/null
  fill $P/c 0 7
  settle
  join $P/c 1/3
  ones "1/3: c wrote 7 MiB, then joined: the group takes what it holds"
  btrfs qgroup remove "0/$(id $P/c)" 1/3 $P
  join $P/c 1/4
  ones "c moved from 1/3 to 1/4"
  btrfs subvolume delete $P/c >/dev/null
  ones "c deleted"
  btrfs qgroup clear-stale $P >/dev/null
  for g in 1 2 3 4; do
    btrfs qgroup destroy 1/$g $P 2>&1 | sed "s|^|   destroy 1/$g: |" && echo "   destroy 1/$g: ok" || true
  done
  btrfs filesystem sync $P
  echo "   what the quota tree holds for the groups left:"
  btrfs inspect-internal dump-tree -t quota /tmp/pool.img | grep -A3 'QGROUP_INFO 1/' | grep -v generation | sed 's/^/   /'
}

limit() {
  say "limit: 150 MiB on a group holding 100 MiB"
  fresh
  btrfs quota enable --simple $P
  mkdir -p $P/envs/e1/snaps
  btrfs qgroup create 1/1 $P
  btrfs subvolume create $P/envs/e1/live >/dev/null
  join $P/envs/e1/live 1/1
  chown 1000:1000 $P/envs/e1/live
  as_box bash -c "for i in \$(seq 0 99); do dd if=/dev/urandom of=$P/envs/e1/live/f\$i bs=1M count=1 status=none; done"
  btrfs qgroup limit 150M 1/1 $P
  show "100 MiB written, limit 150 MiB (157286400 bytes) on 1/1"
  cd $P/envs/e1/live
  # What the writer sees: the error's number, and how far it got.
  hog() {
    as_box perl -e '
      open(my $f, ">", "big") or die "open: $!\n";
      my ($buf, $n) = ("x" x 65536, 0);
      while (1) {
        my $w = syswrite($f, $buf);
        if (!defined $w) { printf "   write(2) failed after %.2f MiB: errno %d, %s\n", $n / 1048576, $! + 0, $!; last }
        $n += $w;
        if ($n > 300 * 1048576) { print "   wrote 300 MiB with no error\n"; last }
      }'
  }
  try() { as_box dd if=/dev/zero of=again bs=1M count=5 2>&1 | grep -v records | sed "s/^/   dd of 5 MiB $1: /" || true; }
  hog
  show "after the failed write"
  echo "what a build sees, as the box's user:"
  as_box dd if=/dev/zero of=dd.out bs=1M count=100 2>&1 | sed 's/^/   dd: /' || true
  as_box cp /usr/local/go/bin/go go.copy 2>&1 | sed 's/^/   cp: /' || true
  as_box bash -c 'echo hello >small' 2>&1 | sed 's/^/   sh: /' || true
  as_box mkdir newdir 2>&1 | sed 's/^/   mkdir: /' || true
  as_box tar -cf - -C /usr/local/go/src fmt 2>/dev/null | as_box tar -xf - 2>&1 | head -1 | sed 's/^/   tar: /' || true
  as_box git init -q repo 2>&1 | head -1 | sed 's/^/   git init: /' || true
  as_box bash -c 'echo more >>f0 && echo ok' 2>&1 | sed 's/^/   append a line to a file that exists: /' || true
  dd if=/dev/zero of=root.out bs=1M count=100 2>&1 | grep -v records | sed 's/^/   dd as root: /' || true
  show "after the build's attempts"
  echo "freeing room, with no snapshot holding the file:"
  as_box rm big && echo "   rm of the 50 MiB file: ok"
  try "right after the rm"
  settle
  try "after the next commit"
  as_box rm -f again
  hog
  echo "what the daemon can still do at the limit:"
  btrfs subvolume snapshot -r $P/envs/e1/live $P/envs/e1/snaps/full >/dev/null &&
    join $P/envs/e1/snaps/full 1/1 && echo "   a snapshot, put into the group: ok" || echo "   a snapshot: failed"
  btrfs subvolume snapshot $P/envs/e1/snaps/full $P/envs/e1/live.new >/dev/null && join $P/envs/e1/live.new 1/1 &&
    btrfs subvolume delete $P/envs/e1/live.new >/dev/null && echo "   a restore's writable copy, put into the group: ok" || echo "   a restore's copy: failed"
  show "after the snapshot and the copy"
  echo "freeing room that a snapshot holds:"
  as_box rm big && echo "   rm of the 50 MiB file: ok"
  settle
  try "after the rm and a commit"
  show "after the rm: the snapshot taken at the limit holds the file"
  btrfs subvolume delete $P/envs/e1/snaps/full >/dev/null
  settle
  try "after the snapshot is deleted"
  show "after the snapshot is deleted"
  cd /
  btrfs qgroup limit none 1/1 $P
  as_box dd if=/dev/zero of=$P/envs/e1/live/again bs=1M count=100 2>&1 | grep -v records | sed 's/^/   dd of 100 MiB with the limit lifted: /' || true
  btrfs qgroup limit 50M 1/1 $P
  show "a limit set below what the group holds: accepted"
  as_box bash -c "echo x >$P/envs/e1/live/small" 2>&1 | sed 's/^/   a write then: /' || true
}

existing() {
  say "existing data: simple quotas enabled on a filesystem that already has an environment"
  fresh
  mkdir -p $P/envs/old/snaps
  btrfs subvolume create $P/envs/old/live >/dev/null
  fill $P/envs/old/live 0 100
  btrfs subvolume snapshot -r $P/envs/old/live $P/envs/old/snaps/s1 >/dev/null
  btrfs filesystem sync $P
  echo "before: btrfs filesystem du says $(btrfs filesystem du -s --raw $P/envs/old | awk 'NR == 2 { print $1 }') bytes"
  local t=$EPOCHREALTIME
  btrfs quota enable --simple $P
  echo "enable --simple took $(((${EPOCHREALTIME/./} - ${t/./}) / 1000)) ms"
  show "enabled: nothing that exists is counted"
  btrfs qgroup create 1/1 $P
  join $P/envs/old/live 1/1
  join $P/envs/old/snaps/s1 1/1
  btrfs qgroup limit 150M 1/1 $P
  show "the existing workspace and snapshot put into 1/1, limit 150 MiB"
  fill $P/envs/old/live 0 50
  show "50 of the old files written again"
  rm $P/envs/old/live/f6? $P/envs/old/live/f7?
  btrfs subvolume delete $P/envs/old/snaps/s1 >/dev/null
  show "20 never-rewritten old files removed and the old snapshot deleted: nothing goes below zero"
  fill $P/envs/old/live 200 120 2>&1 | tail -1 | sed 's/^/   writing 120 MiB more: /' || true
  show "the limit counts only what was written since"
  say "existing data: turning quotas off and on again"
  btrfs quota disable $P
  btrfs quota enable --simple $P
  show "disable, enable --simple: the groups, the limits and every count are gone"
}

# mode puts the filesystem at $P into one of the four ways the timings
# compare, and sets ROOT to the folder the store's pools go in.
mode() {
  btrfs quota disable $P 2>/dev/null || true
  [ ! -d $P/grp ] || btrfs subvolume delete $P/grp >/dev/null
  settle
  ROOT=$P GROUP=
  case $1 in
  off) ;;
  simple) btrfs quota enable --simple $P ;;
  # Every subvolume the store makes lands in one limited group, by the
  # kernel's rule that what is made inside a subvolume joins its groups:
  # the unchanged store, with the upkeep of a group above each subvolume.
  group)
    btrfs quota enable --simple $P
    btrfs qgroup create 1/1 $P
    btrfs subvolume create -i 1/1 $P/grp >/dev/null
    btrfs qgroup limit 100G 1/1 $P
    ROOT=$P/grp GROUP=1/1
    ;;
  classic)
    btrfs quota enable $P
    btrfs quota rescan -w $P >/dev/null
    ;;
  esac
}
MODES=(off simple group classic)
# rounds runs a command once per mode in each of ROUNDS rounds, starting a
# round one mode later than the last, so no mode always follows the same one.
rounds() {
  local r i m
  for ((r = 0; r < ROUNDS; r++)); do
    for ((i = 0; i < ${#MODES[@]}; i++)); do
      m=${MODES[$(((r + i) % ${#MODES[@]}))]}
      mode "$m"
      echo "round $((r + 1)): $m" >&2
      "$@" "$m"
    done
  done
  mode off
}
# table prints, from lines of "mode case value", each case's median per mode
# in ms and its change against off, and how far apart off's own runs were.
table() {
  sort -k2,2 -k1,1 -k3,3n | awk -v modes="${MODES[*]}" '
    function flush(   n, i, m, line, base) {
      if (c == "") return
      n = split(modes, ms, " ")
      line = sprintf("| %-28s |", c)
      for (i = 1; i <= n; i++) {
        m = ms[i]
        if (!(m in cnt)) { line = line " |"; continue }
        med[m] = v[m, int((cnt[m] + 1) / 2)]
        if (m == "off") line = line sprintf(" %.2f | %.2f to %.2f |", med[m] / 1e6, v[m, 1] / 1e6, v[m, cnt[m]] / 1e6)
        else line = line sprintf(" %.2f (%+.0f%%) |", med[m] / 1e6, (med[m] / med["off"] - 1) * 100)
      }
      print line
      delete cnt
    }
    $2 != c { flush(); c = $2 }
    { cnt[$1]++; v[$1, cnt[$1]] = $3 }
    END { flush() }
    BEGIN { print "| ms, median of the runs | off | off, least to most | simple | simple, in a limited group | classic |"; print "|---|---:|---:|---:|---:|---:|" }'
}
# p50s turns `go test -bench` output into "mode case p50-ns" lines.
p50s() {
  awk -v m="$1" '/^Benchmark/ { name = $1; sub(/^Benchmark(Store\/|SpikeBig)/, "", name); sub(/-[0-9]+$/, "", name)
    for (i = 2; i < NF; i++) if ($(i + 1) ~ /p50-ns$/) { unit = $(i + 1); sub(/-?p50-ns$/, "", unit); print m, name unit, $i } }'
}

build_tests() {
  [ ! -x /tmp/store.test ] || return 0
  # A copy, so that the timing below is the only thing added to the store's
  # tests and the repository is left as it is.
  rm -rf /tmp/fern && mkdir /tmp/fern && cp -r /src/go.mod /src/go.sum /src/internal /tmp/fern/
  cat >/tmp/fern/internal/store/spike_test.go <<'EOF'
package store_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/nebler/fern/internal/store"
)

// BenchmarkSpikeBig times one more snapshot, a snapshot's delete and a
// restore in an environment with 20,000 files of 4 KiB and 50 snapshots, 200
// files written again between each. With SPIKE_GROUP (1/N), every subvolume
// is put into that group once it is made, as the design in
// docs/briefs/disk-limit-per-environment.md would, by ioctl.
func BenchmarkSpikeBig(b *testing.B) {
	root := os.Getenv("FERN_TEST_BTRFS")
	dir, err := os.MkdirTemp(root, "big")
	check := func(err error) {
		b.Helper()
		if err != nil {
			b.Fatal(err)
		}
	}
	check(err)
	check(os.Chmod(dir, 0o755))
	ctx := context.WithoutCancel(b.Context())
	st, err := store.Open(ctx, dir)
	check(err)
	store.SetRetention(st, 1000, 0)
	b.Cleanup(func() {
		check(st.Delete(ctx, "big"))
		check(os.RemoveAll(dir))
	})
	var group uint64
	if g := os.Getenv("SPIKE_GROUP"); g != "" {
		n, err := strconv.ParseUint(strings.TrimPrefix(g, "1/"), 10, 48)
		check(err)
		group = 1<<48 | n
	}
	// join puts the subvolume at path into the group: its ID from
	// BTRFS_IOC_INO_LOOKUP, then BTRFS_IOC_QGROUP_ASSIGN.
	join := func(path string) {
		if group == 0 {
			return
		}
		f, err := os.Open(path)
		check(err)
		defer func() { _ = f.Close() }()
		var lookup struct {
			TreeID, ObjectID uint64
			Name             [4080]byte
		}
		lookup.ObjectID = 256
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0xd0009412, uintptr(unsafe.Pointer(&lookup))); errno != 0 {
			b.Fatalf("ino lookup %s: %v", path, errno)
		}
		assign := struct{ Assign, Src, Dst uint64 }{1, lookup.TreeID, group}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0x40189429, uintptr(unsafe.Pointer(&assign))); errno != 0 {
			b.Fatalf("qgroup assign %s: %v", path, errno)
		}
	}
	// cleaned waits until btrfs has dropped every deleted subvolume: a sync
	// pokes its cleaner, and `subvolume list -d` lists what it hasn't done.
	cleaned := func() {
		f, err := os.Open(dir)
		check(err)
		defer func() { _ = f.Close() }()
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0x9408, 0); errno != 0 {
			b.Fatalf("sync: %v", errno)
		}
		for {
			out, err := exec.Command("btrfs", "subvolume", "list", "-d", dir).Output()
			check(err)
			if len(bytes.TrimSpace(out)) == 0 {
				return
			}
		}
	}
	var seed bytes.Buffer
	tw := tar.NewWriter(&seed)
	body := bytes.Repeat([]byte("x"), 4096)
	for i := range 20000 {
		check(tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("f-%05d", i), Mode: 0o644, Size: int64(len(body))}))
		_, err := tw.Write(body)
		check(err)
	}
	check(tw.Close())
	env, err := st.Create(ctx, "big", store.CreateOptions{Seed: &seed})
	check(err)
	join(env.Workspace)
	turn := func(n int) string { return fmt.Sprintf("turn-%04d", n) }
	work := func(n int) {
		for j := range 200 {
			check(os.WriteFile(filepath.Join(env.Workspace, fmt.Sprintf("f-%05d", (n*200+j)%20000)), body, 0o644))
		}
	}
	for n := range 50 {
		work(n)
		check(st.Snapshot(ctx, "big", turn(n)))
		join(st.SnapshotPath("big", turn(n)))
	}
	cleaned()
	samples := make(map[string][]time.Duration)
	timed := func(name string, f func()) {
		start := time.Now()
		f()
		samples[name] = append(samples[name], time.Since(start))
	}
	for i := 0; b.Loop(); i++ {
		n := 50 + i
		work(n)
		timed("snapshot", func() {
			check(st.Snapshot(ctx, "big", turn(n)))
			join(st.SnapshotPath("big", turn(n)))
		})
		timed("delete", func() { check(st.DeleteSnapshot(ctx, "big", turn(i))) })
		timed("delete-cleaned", cleaned)
		timed("restore", func() {
			check(st.Restore(ctx, "big", turn(n-25)))
			join(env.Workspace)
		})
		// The restore's own snapshot of what it replaced is not one of the 50.
		snaps, err := st.Snapshots(ctx, "big")
		check(err)
		for _, s := range snaps {
			if strings.HasPrefix(s.ID, "pre-restore-") {
				check(st.DeleteSnapshot(ctx, "big", s.ID))
			}
		}
		cleaned()
	}
	for name, ds := range samples {
		slices.Sort(ds)
		b.ReportMetric(float64(ds[len(ds)/2].Nanoseconds()), name+"-p50-ns")
		b.ReportMetric(float64(ds[(len(ds)*95-1)/100].Nanoseconds()), name+"-p95-ns")
	}
}
EOF
  (cd /tmp/fern && go test -c -o /tmp/store.test ./internal/store/)
}

LIGHT_CASES='CreateEmpty|SnapshotClean|SnapshotDirty8MiB|DeleteSnapshot|Restore|Reconcile|List|List100|Get|FindByPath|FindByPath100|Snapshots50'
HEAVY_CASES='CreateSeed20000|Adopt20000|BuildSource20000|Delete10Snapshots'
# gotest runs the test binary's benchmarks matching $2 for $3 operations in
# mode $1, with its pools in folder $5 (and each subvolume put into group $6),
# and keeps their medians in file $4.
gotest() {
  FERN_TEST_BTRFS=$5 SPIKE_GROUP=${6:-} /tmp/store.test -test.run '^$' -test.bench "$2" -test.benchtime "${3}x" >/tmp/gotest.out ||
    { cat /tmp/gotest.out >&2; return 1; }
  p50s "$1" </tmp/gotest.out >>"$4"
}
bench_once() {
  gotest "$1" "^BenchmarkStore\$/^($LIGHT_CASES)\$" "$LIGHT" /tmp/bench.txt "$ROOT"
  gotest "$1" "^BenchmarkStore\$/^($HEAVY_CASES)\$" "$HEAVY" /tmp/bench.txt "$ROOT"
}
bench() {
  say "the store's benchmark, $ROUNDS runs per mode ($LIGHT operations a run; $HEAVY for the cases that make 20,000 files)"
  build_tests
  fresh
  # Once unmeasured: the first run on a new filesystem is the slowest.
  mode off
  bench_once off
  : >/tmp/bench.txt
  rounds bench_once
  table </tmp/bench.txt
}

# The pool is a plain folder here, and the test puts each subvolume into the
# group itself once it is made: the way the brief proposes.
big_once() { gotest "$1" '^BenchmarkSpikeBig$' "$BIG" /tmp/big.txt $P "$GROUP"; }
big() {
  say "20,000 files and 50 snapshots, $ROUNDS runs per mode ($BIG operations a run)"
  build_tests
  fresh
  mode off
  BIG=3 big_once off
  : >/tmp/big.txt
  rounds big_once
  table </tmp/big.txt
}

apt-get update -qq && apt-get install -y -qq btrfs-progs >/dev/null
for section in "$@"; do "$section"; done
