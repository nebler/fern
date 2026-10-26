# shellcheck shell=bash
# Host-side helpers. Source after lib.sh (check.sh and t3-run.sh).
hardware_uids() {
  HW_DAEMON=$(k -n fern-system get pod -l app.kubernetes.io/name=fern -o jsonpath='{.items[0].metadata.uid}')
  HW_T3=$(k -n fern-system get pod -l app.kubernetes.io/name=t3 -o jsonpath='{.items[0].metadata.uid}')
  HW_CONTROLLER=$(k -n agent-sandbox-system get pods -o jsonpath='{.items[0].metadata.uid}')
}
hardware_sample() {
  on_node sh -s -- sample "$HW_DAEMON" "$HW_T3" "$HW_CONTROLLER" "$@" <lab/hardware-node.sh |
    awk '{v[$1]+=$2} END {for(k in v) printf "%s %.3f\n",k,v[k]}' || {
      echo "FAIL hardware sample" >&2
      return 1
    }
}
# resource_op preserves the CLI's output and adds metrics for its node-local
# interval. No SSH/kubectl round trip is inside that interval.
resource_op() {
  local key=$1 out name value
  shift
  [ -n "${HW_DAEMON:-}" ] || HW_DAEMON=$(k -n fern-system get pod -l app.kubernetes.io/name=fern -o jsonpath='{.items[0].metadata.uid}')
  out=$(on_node sh -s -- op "$HW_DAEMON" "$FERN_CLI" "$@" <lab/hardware-node.sh) || { echo "FAIL $key resource operation" >&2; return 1; }
  while read -r _ name value; do
    metric "$key.$name" "$key $name" "$value" >&2
  done < <(sed -n '/^RESOURCE /p' <<<"$out")
  if [ "$1" = wake ]; then
    local uid pod_cpu pod_io
    uid=$(pod_uid "$2")
    if [ -z "$uid" ] || ! pod_cpu=$(pod_cgroups cpu.stat "$uid") || ! pod_io=$(pod_cgroups io.stat "$uid"); then
      echo "FAIL $key reading wake pod counters" >&2
      return 1
    fi
    value=$(awk '$1=="usage_usec" {printf "%.3f", $2/1000}' <<<"$pod_cpu")
    metric "$key.box_cpu_ms" "$key pod CPU since birth, ms (gVisor included)" "$value" >&2 || return
    value=$(awk '{for(i=2;i<=NF;i++) if($i ~ /^wbytes=/) {split($i,a,"=");b+=a[2]}} END {printf "%.0f",b}' <<<"$pod_io")
    metric "$key.box_write_bytes" "$key pod charged writes since birth, bytes" "$value" >&2 || return
  fi
  sed '/^RESOURCE /d' <<<"$out"
}
# resource_rates uses real elapsed time to normalize a short idle window to
# CPU-ms/minute. Cumulative counters stay in their cgroups, not in a pod exec.
resource_rates() {
  local key=$1 before=$2 after=$3 boxes=${4:-1}
  awk -v key="$key" -v boxes="$boxes" '
    NR==FNR {b[$1]=$2;next} {a[$1]=$2}
    END {dt=a["elapsed_ms"]-b["elapsed_ms"]; if(dt<=0) exit 1
      for(k in a) if(k ~ /_(cpu|throttled)_ms$/ && k in b && a[k]>=b[k])
        printf "%s.%s_per_min_ms %.3f\n",key,substr(k,1,length(k)-3),(a[k]-b[k])*60000/dt/(k=="box_cpu_ms"?boxes:1)
    }' "$before" "$after" | while read -r name value; do metric "$name" "$name" "$value"; done
}
# btrfs du reports allocated data extents, including sharing. Exclusive
# bytes measure only unshared data; metadata and physical device writes
# aren't attributed by this command. Pass every snapshot together so the
# set-shared column counts their common extents once.
pool_du() {
  local key=$1 path=$2 out
  out=$(in_daemon "btrfs filesystem du -s --raw $path") || { echo "FAIL $key btrfs accounting" >&2; return 1; }
  awk 'NR>1 {total+=$1; exclusive+=$2; shared+=$3}
    END {printf "referenced_bytes %.0f\nexclusive_bytes %.0f\nset_bytes %.0f\n",total,exclusive,exclusive+shared}' <<<"$out" |
    while read -r name value; do metric "$key.$name" "$key $name" "$value"; done
}
# stability reports absolute endpoints and positive retained growth. A drop
# in used memory isn't a negative cost, and must not become a false baseline.
resource_stability() {
  local key=$1 before=$2 after=$3
  awk -v key="$key" 'NR==FNR {b[$1]=$2;next}
    $1 ~ /^(node_used_mib|daemon_rss_mib|daemon_anon_mib|daemon_fds_count|daemon_threads_count|daemon_goroutines_count)$/ {
      unit=$1; sub(/.*_/,"",unit); name=$1; sub(/_[^_]*$/,"",name)
      printf "%s.%s_before_%s %.3f\n%s.%s_after_%s %.3f\n%s.%s_growth_%s %.3f\n",key,name,unit,b[$1],key,name,unit,$2,key,name,unit,($2>b[$1]?$2-b[$1]:0)
    }' "$before" "$after" | while read -r name value; do metric "$name" "$name" "$value"; done
}
