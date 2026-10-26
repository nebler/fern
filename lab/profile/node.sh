# shellcheck shell=sh
# Runs on the node, sent by lab/profile.sh: node.sh OP ENV N HZ DIR. Makes
# operation OP on environment ENV N times, each after its untimed prepare
# step, and records the node around each one with perf: CPU samples at HZ
# and scheduler switches. fold.pl (in DIR's parent) then keeps what fell
# inside each operation's window. Writes DIR/times (start and end of each
# window, seconds since the epoch, to match traces), DIR/mono (the same on
# the monotonic clock, which perf uses: the kernel stamps tracepoints with no
# other), and DIR/oncpu.folded and DIR/offcpu.folded, or neither without
# perf.
set -eu
op=$1 env=$2 n=$3 hz=$4 dir=$5
cli=/usr/local/bin/fern-cli
here=$(dirname "$dir")
rm -rf "$dir"
mkdir -p "$dir"
: >"$dir/mono"
perf=$(command -v perf || true)
# mono sets $mono to the monotonic clock, in seconds, with builtins only, so
# no process of its own lands in the window: timer_list's third line is
# "now at NS nsecs".
mono() {
  {
    read -r _
    read -r _
    read -r _ _ ns _
  } </proc/timer_list
  mono=$((ns / 1000000000)).$(printf %09d $((ns % 1000000000)))
}

prepare() {
  case $op in
    wake) $cli sleep "$env" ;;
    *) $cli wake "$env" ;;
  esac >/dev/null
}

measure() {
  case $op in
    wake) $cli wake "$env" ;;
    sleep) $cli sleep "$env" ;;
    restore) $cli restore "$env" prof ;;
    snapshot) $cli snapshot --replace "$env" prof ;;
    exec) $cli exec "$env" -- true ;;
    *) echo "unknown operation $op" >&2 && exit 2 ;;
  esac >/dev/null
}

$cli wake "$env" >/dev/null
$cli snapshot --replace "$env" prof >/dev/null
# The wall clock less the monotonic one: windows are read on the monotonic
# clock alone, and get wall-clock times from this at the end.
mono
offset=$(awk -v m="$mono" -v d="$(date +%s.%N)" 'BEGIN {printf "%.6f", d - m}')
i=0
while [ "$i" -lt "$n" ]; do
  i=$((i + 1))
  prepare
  # The prepare step's own work (a pod going away) ends before the window.
  sleep 1
  if [ -n "$perf" ]; then
    "$perf" record -q -a -g -k CLOCK_MONOTONIC -e cpu-clock -F "$hz" -o "$dir/on.$i.data" 2>/dev/null &
    on=$!
    "$perf" record -q -a -g -k CLOCK_MONOTONIC -e sched:sched_switch -o "$dir/off.$i.data" 2>/dev/null &
    off=$!
    sleep 0.5
  fi
  mono
  start=$mono
  measure
  mono
  echo "$start $mono" >>"$dir/mono"
  if [ -n "$perf" ]; then
    kill -INT "$on" "$off"
    wait "$on" "$off" || true
  fi
done

awk -v o="$offset" '{printf "%.6f %.6f\n", $1 + o, $2 + o}' "$dir/mono" >"$dir/times"
[ -n "$perf" ] || exit 0
i=0
: >"$dir/on.script"
: >"$dir/off.script"
while [ "$i" -lt "$n" ]; do
  i=$((i + 1))
  window=$(sed -n "${i}p" "$dir/mono")
  # --no-inline: inlined frames need addr2line, which a node may not have.
  # shellcheck disable=SC2086 # the window is two arguments
  "$perf" script --no-inline -i "$dir/on.$i.data" -F comm,tid,time,ip,sym,dso 2>/dev/null |
    perl "$here/fold.pl" on $window >>"$dir/on.script"
  # shellcheck disable=SC2086
  "$perf" script --no-inline -i "$dir/off.$i.data" -F comm,tid,time,trace,ip,sym,dso 2>/dev/null |
    perl "$here/fold.pl" off $window >>"$dir/off.script"
  rm -f "$dir/on.$i.data" "$dir/off.$i.data"
done
# Sum the windows' stacks.
for k in on off; do
  awk '{v=$NF; $NF=""; sub(/ $/,""); s[$0]+=v} END {for (k in s) print k, s[k]}' "$dir/$k.script" |
    sort >"$dir/${k}cpu.folded"
  rm -f "$dir/$k.script"
done
