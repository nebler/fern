# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section sleeping of lab/check.sh, which sources this file: it has no use by itself.
# sleeping: how many asleep threads a node holds. A thread with Postgres
# forks into 8, each of those into 8, and so on: 585 environments that share
# their data as threads from one golden do, each with a Postgres data folder
# of its own. Asleep, a thread is files and a row; this measures what they
# cost the disk, the daemon and its answers, and a wake among them.
sleeping() {
  echo "== sleeping threads"
  fresh $ENV
  sql $ENV 'select count(*) from users' >/dev/null
  fern sleep $ENV >/dev/null
  in_daemon "btrfs filesystem sync /var/lib/fern" >/dev/null
  local disk0 rss0 start total
  disk0=$(in_daemon "df --output=used -B1M /var/lib/fern | tail -1" | tr -d ' ')
  # The process's own memory: its cgroup's also counts the page cache its
  # snapshots read.
  rss0=$(in_daemon 'ps -o rss= -p "$(systemctl show -P MainPID fern-daemon)"')
  start=$(now_ms)
  # On the node: one SSH session, not 73.
  # shellcheck disable=SC2016 # the node's shell's
  on_node bash -c 'set -e; level=("$1"); for _ in 1 2 3; do next=(); for e in "${level[@]}"; do
      mapfile -t -O "${#next[@]}" next < <("$2" fork -n 8 "$e" 2>/dev/null); done; level=("${next[@]}"); done' bash $ENV "$FERN_CLI" </dev/null
  total=$(fern ls | grep -c "^$ENV" || true)
  check "585 threads (1 + 8 + 64 + 512)" 585 "$total"
  metric sleeping.fork584_ms "584 forks made, ms" $(($(now_ms) - start))
  in_daemon "btrfs filesystem sync /var/lib/fern" >/dev/null
  metric sleeping.disk_per_thread_kib "disk per asleep thread, KiB (metadata: its data is shared until written)" \
    "$(($(($(in_daemon "df --output=used -B1M /var/lib/fern | tail -1" | tr -d ' ') - disk0)) * 1024 / 584))"
  metric sleeping.daemon_memory_mib "the daemon's memory (RSS) with 585 asleep threads, MiB more than with 1" \
    "$(awk -v a="$(in_daemon 'ps -o rss= -p "$(systemctl show -P MainPID fern-daemon)"')" -v b="$rss0" 'BEGIN { printf "%.1f", (a - b) / 1024 }')"
  metric sleeping.ls_ms "fern ls with 585 threads, ms" "$(node_ms "$FERN_CLI" ls)"
  local deep=$ENV-fork-5-fork-3-fork-7
  metric sleeping.wake_ms "a wake among them, ms" "$(fern wake $deep | took)"
  check "it has its own Postgres with the data (3 users)" 3 "$(sql $deep 'select count(*) from users')"
  start=$(now_ms)
  restart_daemon
  metric sleeping.restart_ms "daemon restart with 585 threads, ms until it answers" $(($(now_ms) - start))
  check "the threads are all there after it" 585 "$(fern ls | grep -c "^$ENV" || true)"
  start=$(now_ms)
  fern rm $ENV
  metric sleeping.delete585_ms "the thread and its 584 forks deleted, ms" $(($(now_ms) - start))
  check "none is left" 0 "$(fern ls | grep -c "^$ENV" || true)"
}
