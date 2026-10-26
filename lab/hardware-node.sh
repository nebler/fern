#!/bin/sh
# Read-only Linux cgroup-v2/proc sampler, sent to on_node's sh on stdin.
# UID arguments avoid Kubernetes calls in operation and turn measurements.
set -eu
cgroup() {
  slice=$(printf %s "$1" | tr - _)
  for dir in /sys/fs/cgroup/kubepods*/pod"$1" /sys/fs/cgroup/kubepods*/*/pod"$1" \
    /sys/fs/cgroup/kubepods*/*/*pod"$slice".slice /sys/fs/cgroup/kubepods*/*pod"$slice".slice; do
    [ -r "$dir/cpu.stat" ] || continue
    printf '%s\n' "$dir"
    return
  done
  return 1
}
network() {
  iface=$(awk '$2=="00000000" {print $1; exit}' /proc/net/route)
  awk -v iface="$iface:" '$1==iface {print $2,$10}' /proc/net/dev
}
node_cpu() {
  # Linux exports USER_HZ (100), independent of the kernel's CONFIG_HZ.
  awk '/^cpu / { printf "%.0f\n", ($2+$3+$4+$7+$8+$9)*10; exit }' /proc/stat
}
usage() { awk '$1 == "usage_usec" { printf "%.3f\n", $2/1000 }' "$1/cpu.stat"; }
writes() {
  awk '{ for (i=2;i<=NF;i++) if ($i ~ /^wbytes=/) { split($i,a,"="); b+=a[2] } }
    END { printf "%.0f\n", b }' "$1/io.stat"
}
pod_sample() {
  name=$1
  cg=$(cgroup "$2") || { echo "missing cgroup for $name" >&2; return 1; }
  echo "${name}_cpu_ms $(usage "$cg")"
  awk -v n="$name" '$1 == "throttled_usec" { printf "%s_throttled_ms %.3f\n", n, $2/1000 }' "$cg/cpu.stat"
  echo "${name}_write_bytes $(writes "$cg")"
  awk -v n="$name" '$1 == "anon" || $1 == "shmem" { b+=$2 }
    $1 == "kernel" { k=$2 } END { printf "%s_memory_mib %.3f\n%s_kernel_mib %.3f\n", n,b/1048576,n,k/1048576 }' "$cg/memory.stat"
}
process_sample() {
  # k3s embeds kubelet/apiserver; containerd is its own child. Do not count
  # both k3s's parent cgroup and containerd's child cgroup as separate costs.
  for proc in /proc/[0-9]*; do
    comm=$(cat "$proc/comm" 2>/dev/null) || continue
    case $comm in
      k3s|k3s-server) name=k3s ;;
      exe) case $(tr '\000' ' ' <"$proc/cmdline" 2>/dev/null) in *'k3s server'*) name=k3s ;; *) continue ;; esac ;;
      containerd) name=containerd ;;
      fern-daemon) name=daemon ;;
      *) continue ;;
    esac
    # Strip comm, which may contain spaces, before addressing stat fields.
    sed 's/.*) //' "$proc/stat" 2>/dev/null | awk -v n="$name" '{ printf "%s_process_cpu_ms %.0f\n", n,($12+$13)*10 }'
    awk -v n="$name" '$1=="VmRSS:" {printf "%s_rss_mib %.3f\n",n,$2/1024}
      $1=="RssAnon:" {printf "%s_anon_mib %.3f\n",n,$2/1024}
      $1=="Threads:" {printf "%s_threads_count %d\n",n,$2}' "$proc/status" 2>/dev/null || true
    if [ "$name" = daemon ]; then
      echo "daemon_fds_count $(find "$proc/fd" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l)"
    fi
  done | awk '{ v[$1]+=$2 } END {
    for(k in v) printf "%s %.3f\n",k,v[k]
    if (!("k3s_rss_mib" in v) || !("containerd_rss_mib" in v) || !("daemon_rss_mib" in v)) exit 1
  }'
}
mode=$1
shift
case $mode in
  op)
    cg=$(cgroup "$1")
    shift
    read -r before_rx before_tx <<EOF
$(network)
EOF
    before_node=$(node_cpu) before_cpu=$(usage "$cg") before_io=$(writes "$cg")
    "$@"
    after_node=$(node_cpu) after_cpu=$(usage "$cg") after_io=$(writes "$cg")
    read -r after_rx after_tx <<EOF
$(network)
EOF
    awk -v br="$before_rx" -v ar="$after_rx" -v bt="$before_tx" -v at="$after_tx" \
      'BEGIN {printf "RESOURCE node_rx_bytes %.0f\nRESOURCE node_tx_bytes %.0f\n",ar-br,at-bt}'
    awk -v bn="$before_node" -v an="$after_node" -v bc="$before_cpu" -v ac="$after_cpu" -v bi="$before_io" -v ai="$after_io" \
      'BEGIN { printf "RESOURCE node_cpu_ms %.3f\nRESOURCE daemon_cpu_ms %.3f\nRESOURCE daemon_write_bytes %.0f\n",an-bn,ac-bc,ai-bi }'
    ;;
  sample)
    awk '{ printf "elapsed_ms %.0f\n", $1*1000 }' /proc/uptime
    echo "node_cpu_ms $(node_cpu)"
    awk '/^MemTotal:/ {t=$2} /^MemAvailable:/ {a=$2} END {printf "node_used_mib %.3f\n",(t-a)/1024}' /proc/meminfo
    pod_sample daemon "$1"
    pod_sample t3 "$2"
    pod_sample controller "$3"
    shift 3
    for uid do
      # A restore can remove a box between metadata lookup and this read.
      if cgroup "$uid" >/dev/null; then pod_sample box "$uid"; fi
    done
    process_sample
    ;;
  *) echo "unknown hardware sampler mode: $mode" >&2; exit 2 ;;
esac
