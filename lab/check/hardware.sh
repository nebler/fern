# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section hardware of lab/check.sh, which sources this file: it has no use by itself.
hardware() {
  echo "== hardware: idle CPU, fixed platform cost and allocated pool extents"
  local uid empty_uid before_bytes after_bytes seconds=${CPU_IDLE_SECONDS:-15}
  hardware_uids
  hardware_sample >"$TMP/platform-before"
  sleep "$seconds"
  hardware_sample >"$TMP/platform-after"
  resource_rates hardware.platform_idle "$TMP/platform-before" "$TMP/platform-after"
  while read -r name value; do
    case $name in
      t3_*mib|daemon_*mib|daemon_*count)
        metric "hardware.platform.$name" "idle platform $name" "$value" ;;
    esac
  done <"$TMP/platform-after"
  fresh $ENV
  resource_op box.fresh_wake wake $ENV >/dev/null
  fern rm ${ENV}-empty 2>/dev/null || true
  fern create ${ENV}-empty >/dev/null
  fern wake ${ENV}-empty >/dev/null
  uid=$(pod_uid $ENV) empty_uid=$(pod_uid ${ENV}-empty)
  # Let initdb's startup activity settle before the idle interval.
  sleep 2
  hardware_sample --counters "$uid" >"$TMP/pg-before"
  hardware_sample --counters "$empty_uid" >"$TMP/empty-before"
  sleep "$seconds"
  hardware_sample --counters "$uid" >"$TMP/pg-after"
  hardware_sample --counters "$empty_uid" >"$TMP/empty-after"
  resource_rates hardware.postgres_idle "$TMP/pg-before" "$TMP/pg-after"
  resource_rates hardware.empty_idle "$TMP/empty-before" "$TMP/empty-after"
  fern rm ${ENV}-empty
  # btrfs du cannot see delayed allocations. Flush initdb and seed writes
  # before attributing any later allocation growth to checkpoints.
  in_daemon 'sync -f /var/lib/fern'
  pool_du hardware.environment "/var/lib/fern/envs/$ENV/live"
  before_bytes=$(in_daemon "btrfs filesystem du -s --raw /var/lib/fern/envs/$ENV" | awk 'NR>1 {print $2+$3}')
  for i in $(seq 5); do
    fern exec $ENV -- sh -c 'head -c 1048576 /dev/urandom > checkpoint-data'
    resource_op hardware.snapshot snapshot $ENV "disk-$i" >/dev/null
  done
  in_daemon 'sync -f /var/lib/fern'
  pool_du hardware.snapshot "/var/lib/fern/envs/$ENV/snaps/disk-5"
  pool_du hardware.environment_with_snapshots "/var/lib/fern/envs/$ENV"
  after_bytes=$(in_daemon "btrfs filesystem du -s --raw /var/lib/fern/envs/$ENV" | awk 'NR>1 {print $2+$3}')
  metric hardware.checkpoint_growth_bytes "allocated data growth/checkpoint (1 MiB replaced each time), bytes" \
    "$(awk -v a="$after_bytes" -v b="$before_bytes" 'BEGIN {printf "%.0f",(a-b)/5}')"
  # Counter reads and pod lookups change the cadence between wakes and
  # sleeps. Keep CPU samples separate from box's original timing loop.
  for _ in $(seq 3); do
    resource_op box.sleep sleep $ENV >/dev/null
    resource_op box.wake wake $ENV >/dev/null
  done
}
