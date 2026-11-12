#!/usr/bin/env bash
# The node's half of the pause spike (run.sh sends it over stdin):
#   node.sh FUNCTION ENV [ARGS…]
# Everything it reads is the node's own: runsc's state, the box's cgroup and
# the box's address.
set -euo pipefail
R=(runsc --root /run/fern/runsc)

now() { echo $(($(date +%s%N) / 1000000)); }
# id prints the runsc ID of environment $1's box.
id() { "${R[@]}" list 2>/dev/null | awk -v b="/run/fern/boxes/$1" '$4 == b { print $1 }'; }
# status prints runsc's word for the box: running, paused, stopped.
status() { "${R[@]}" list 2>/dev/null | awk -v b="/run/fern/boxes/$1" '$4 == b { print $3 }'; }
ip() { awk '{ n = $1 * 4 + 2; printf "10.89.%d.%d\n", int(n / 256), n % 256 }' "/run/fern/boxes/$1/net"; }
cg() { echo "/sys/fs/cgroup/fern/$1"; }

# ready_ms waits until fern-init in the box answers /readyz, and prints how
# long that took.
ready_ms() {
  local s
  s=$(now)
  until curl -sf -o /dev/null --max-time 1 "http://$(ip "$1"):7070/readyz"; do
    [ $(($(now) - s)) -lt 20000 ] || { echo timeout; return; }
  done
  echo $(($(now) - s))
}

# cycle pauses and resumes the box $2 times, holding each pause for $3
# seconds, and prints for each: pause ms, resume ms, then ms until fern-init
# answers.
cycle() {
  local box n=${2:-10} hold=${3:-1} a b c
  box=$(id "$1")
  for _ in $(seq "$n"); do
    a=$(now)
    "${R[@]}" pause "$box"
    b=$(now)
    sleep "$hold"
    c=$(now)
    "${R[@]}" resume "$box"
    echo "$((b - a)) $(($(now) - c)) $(ready_ms "$1")"
  done
}

pause() { "${R[@]}" pause "$(id "$1")"; }
resume() { "${R[@]}" resume "$(id "$1")"; }

# cpu prints the CPU the box's cgroup used over $2 seconds, in ms.
cpu() {
  local a
  a=$(awk '$1 == "usage_usec" { print $2 }' "$(cg "$1")/cpu.stat")
  sleep "$2"
  awk -v a="$a" '$1 == "usage_usec" { printf "%.1f\n", ($2 - a) / 1000 }' "$(cg "$1")/cpu.stat"
}

# mem prints the box's memory in MiB: what the cgroup holds, the two parts
# Fern counts (anon and shmem), the page cache, and what is in swap.
mem() {
  local c
  c=$(cg "$1")
  awk -v cur="$(cat "$c/memory.current")" -v sw="$(cat "$c/memory.swap.current" 2>/dev/null || echo 0)" '
    $1 == "anon" { a = $2 } $1 == "shmem" { s = $2 } $1 == "file" { f = $2 }
    END { printf "current=%.1f anon=%.1f shmem=%.1f file=%.1f swap=%.1f\n", cur / 1048576, a / 1048576, s / 1048576, f / 1048576, sw / 1048576 }' "$c/memory.stat"
}

# reclaim asks the kernel to take $2 (such as 1G) of the box's memory back:
# to swap, if the node has any.
reclaim() {
  echo "$2" >"$(cg "$1")/memory.reclaim" 2>&1 || echo "reclaim: the kernel took less than $2"
}

swaps() { cat /proc/swaps; grep -E '^(SwapTotal|SwapFree|MemAvailable)' /proc/meminfo; uname -r; }

"$@"
