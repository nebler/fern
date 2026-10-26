#!/usr/bin/env bash
# Shows where an operation's time and CPU go, as flamegraphs and traces:
#
#   lab/profile.sh on            turn tracing on: Jaeger, and traces from the
#                                daemon and kubelet (k3s restarts)
#   lab/profile.sh run OP [N]    profile OP (wake, sleep, restore, snapshot,
#                                exec) N times (default 10)
#   lab/profile.sh diff A B      what changed from profile A to profile B
#   lab/profile.sh ui            open Jaeger's UI (http://localhost:16686)
#   lab/profile.sh off           turn tracing off again
#
# A run writes lab/profiles/<time>-<host>-<op>/: summary.md (times, CPU by
# process, the steps' times, the median operation's timeline), oncpu.svg and
# offcpu.svg (the whole node, kernel included: what ran, and what waited for
# what), their .folded stacks, the daemon's Go CPU profile (daemon.pprof,
# for "go tool pprof") and Jaeger's traces. k3s's containerd is built
# without tracing; kubelet's spans time each of its calls to it, and the
# flamegraphs show its CPU and runsc's. Flamegraphs need perf, which "on"
# installs on the VPS; the local lab has traces and the daemon's profile.
# Tracing slows what it watches a little, so lab/perf.sh refuses to run
# while it is on.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

# FlameGraph's scripts render the stacks, on this machine.
FLAMEGRAPH_REF=41fee1f99f9276008b7cd112fca19dc3ea84ac32
FG=lab/.cache/FlameGraph-$FLAMEGRAPH_REF
REMOTE=/tmp/fern-profile
ENV_ID=ck-prof
HZ=${HZ:-499}

flamegraph() {
  if [ ! -x "$FG/flamegraph.pl" ]; then
    mkdir -p "$FG"
    curl -fsSL "https://github.com/brendangregg/FlameGraph/archive/$FLAMEGRAPH_REF.tar.gz" |
      tar -xz -C "$FG" --strip-components 1
  fi
  "$FG/$1" "${@:2}"
}

# send copies file $1 to the node, as $2.
send() { on_node sh -c "mkdir -p '$(dirname "$2")' && cat >'$2'" <"$1"; }

# daemon_get fetches path $1 from the daemon's metrics port.
daemon_get() {
  local pod
  pod=$(k -n fern-system get pod -l app.kubernetes.io/name=fern -o jsonpath='{.items[0].metadata.name}')
  k get --raw "/api/v1/namespaces/fern-system/pods/$pod:9090/proxy$1"
}

tracing_on() { k -n fern-system get deploy/jaeger >/dev/null 2>&1; }

# rfc3339 prints Unix time $1 as Jaeger's queries take it.
rfc3339() { date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }

# trace_arg prints the index of the daemon's -trace-endpoint argument, if it
# has one. The daemon is the DaemonSet's first container.
trace_arg() {
  k -n fern-system get ds/fern -o jsonpath='{.spec.template.spec.containers[0].args}' |
    tr -d '[]"' | tr ',' '\n' | awk '/^-trace-endpoint=/ {print NR - 1}'
}

KUBELET_TRACING=/var/lib/rancher/k3s/agent/etc/kubelet.conf.d/50-fern-tracing.conf

on() {
  k apply -f - <lab/profile/jaeger.yaml >/dev/null
  k -n fern-system rollout status deploy/jaeger --timeout=180s >/dev/null
  daemon_tracing
  if [ -n "$FERN_HOST" ]; then
    # kubelet traces too, which needs k3s to restart; its boxes keep
    # running. perf records the node for the flamegraphs.
    on_node sh -c "command -v perf >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq linux-tools-\$(uname -r) linux-tools-common >/dev/null"
    on_node sh -c "cat >$KUBELET_TRACING" <<'EOF'
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
tracing:
  endpoint: localhost:4317
  samplingRatePerMillion: 1000000
EOF
    restart_k3s
  else
    echo "profile: the local lab traces the daemon only: kubelet needs a restart of its node" >&2
  fi
  echo "profile: tracing is on; lab/profile.sh off turns it off"
}

off() {
  k -n fern-system delete deploy/jaeger --ignore-not-found >/dev/null
  if [ -n "$FERN_HOST" ]; then
    on_node rm -f $KUBELET_TRACING
    restart_k3s
  fi
  local i
  i=$(trace_arg)
  if [ -n "$i" ]; then
    k -n fern-system patch ds/fern --type=json \
      -p "[{\"op\":\"remove\",\"path\":\"/spec/template/spec/containers/0/args/$i\"}]" >/dev/null
  fi
  k -n fern-system rollout status ds/fern --timeout=180s >/dev/null
  echo "profile: tracing is off"
}

# daemon_tracing makes the daemon send its spans to Jaeger, at its node's
# address (HOST_IP).
daemon_tracing() {
  if [ -z "$(trace_arg)" ]; then
    # shellcheck disable=SC2016 # $(HOST_IP) is Kubernetes's to expand
    k -n fern-system patch ds/fern --type=json \
      -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"-trace-endpoint=http://$(HOST_IP):4318"}]' >/dev/null
  fi
  k -n fern-system rollout status ds/fern --timeout=180s >/dev/null
}

restart_k3s() {
  on_node systemctl restart k3s
  for _ in $(seq 60); do
    k get --raw /readyz >/dev/null 2>&1 && break
    sleep 2
  done
  k wait --for=condition=Ready node --all --timeout=120s >/dev/null
}

run() {
  local op=$1 n=${2:-10} out
  case $op in wake | sleep | restore | snapshot | exec) ;; *) echo "profile: unknown operation $op" >&2 && exit 2 ;; esac
  lab_lock
  # lab/build.sh puts the daemon back as deploy/fern.yaml has it, untraced.
  if tracing_on && [ -z "$(trace_arg)" ]; then daemon_tracing; fi
  out=lab/profiles/$(date -u +%Y%m%dT%H%M%SZ)-${FERN_HOST:-local}-$op
  mkdir -p "$out"
  fern rm $ENV_ID 2>/dev/null || true
  fern create $ENV_ID --repo lab/sample-repo >/dev/null
  send lab/profile/node.sh $REMOTE/node.sh
  send lab/profile/fold.pl $REMOTE/fold.pl
  local from to
  from=$(date +%s)
  # The daemon's own profile, over about the run's length.
  daemon_get "/debug/pprof/profile?seconds=$((n * 4 + 5))" >"$out/daemon.pprof" &
  local pprof=$!
  on_node sh $REMOTE/node.sh "$op" $ENV_ID "$n" "$HZ" $REMOTE/out
  wait $pprof || echo "profile: the daemon's CPU profile failed" >&2
  to=$(date +%s)
  on_node tar -C $REMOTE/out -cf - . | tar -xf - -C "$out"
  fern rm $ENV_ID >/dev/null

  {
    echo "# Profile: $op × $n on ${FERN_HOST:-the local lab}, $(date -u +%FT%TZ)"
    echo
    echo "Fern $(git rev-parse --short HEAD)$(git diff --quiet HEAD -- internal cmd || echo ' (uncommitted changes)')."
    echo "Each $op ran after its untimed prepare step and a second's pause, with perf recording"
    echo "(on the VPS), which slows it a little: lab/perf.sh's numbers are the ones to compare."
    echo
    echo "## Wall time"
    echo
    awk '{printf "%.1f\n", ($2 - $1) * 1000}' "$out/times" | sort -n |
      awk '{t[NR] = $1; s += $1} END {printf "n=%d mean=%.1f ms, min %.1f, p50 %.1f, max %.1f; all: ", NR, s / NR, t[1], t[int((NR + 1) / 2)], t[NR]
        for (i = 1; i <= NR; i++) printf "%s ", t[i]; print ""}' 
  } >"$out/summary.md"

  if [ -s "$out/oncpu.folded" ]; then
    symbolize "$out"
    local windows_s
    windows_s=$(awk '{s += $2 - $1} END {printf "%.3f", s}' "$out/times")
    flamegraph flamegraph.pl --title "$op × $n: CPU on the node" --countname samples \
      --subtitle "$HZ Hz samples of every CPU, within the operations' windows" "$out/oncpu.folded" >"$out/oncpu.svg"
    flamegraph flamegraph.pl --title "$op × $n: time off the CPU" --countname us --colors io \
      --subtitle "waits that ended in the operations' windows" "$out/offcpu.folded" >"$out/offcpu.svg"
    {
      echo
      echo "## CPU by process, within the windows ($windows_s s of wall time)"
      echo
      echo "The node's CPUs idled for the rest (swapper): $(awk -v hz="$HZ" -v n="$n" '/^swapper;/ {s += $NF}
        END {printf "%.0f ms, %.1f per %s", s * 1000 / hz, s * 1000 / hz / n, op}' op="$op" "$out/oncpu.folded")."
      echo
      echo "| process | CPU ms | per $op, ms |"
      echo "|---|---|---|"
      awk -v hz="$HZ" -v n="$n" '!/^swapper;/ {c = $0; sub(/;.*/, "", c); s[c] += $NF; t += $NF}
        END {for (c in s) printf "| %s | %.0f | %.1f |\n", c, s[c] * 1000 / hz, s[c] * 1000 / hz / n
          printf "| **all** | %.0f | %.1f |\n", t * 1000 / hz, t * 1000 / hz / n}' "$out/oncpu.folded" |
        sort -t'|' -k3 -rn | awk 'NR <= 25'
      echo
      echo "## Hottest functions, by own CPU"
      echo
      echo "| function | process | CPU ms |"
      echo "|---|---|---|"
      awk -v hz="$HZ" '!/^swapper;/ {v = $NF; $NF = ""; n = split($0, f, ";"); leaf = f[n]; sub(/ $/, "", leaf)
        k = leaf " | " f[1]; s[k] += v} END {for (k in s) printf "| %s | %.0f |\n", k, s[k] * 1000 / hz}' "$out/oncpu.folded" |
        sort -t'|' -k4 -rn | awk 'NR <= 20'
    } >>"$out/summary.md"
  else
    echo -e "\nNo flamegraphs: the node has no perf." >>"$out/summary.md"
  fi

  if [ -s "$out/daemon.pprof" ]; then
    {
      echo
      echo "## The daemon's CPU (go tool pprof -top)"
      echo
      echo '```'
      "${GO_RUN[@]}" golang:1.27 go tool pprof -top -nodecount=15 "$out/daemon.pprof" 2>/dev/null | awk 'NR <= 25' || true
      echo '```'
    } >>"$out/summary.md"
  fi

  if tracing_on; then
    # Spans arrive in batches, every few seconds.
    sleep 6
    local query
    for service in fern-daemon kubelet; do
      query="query.service_name=$service&query.start_time_min=$(rfc3339 $((from - 5)))&query.start_time_max=$(rfc3339 $((to + 5)))&query.num_traces=5000"
      on_node sh -c "curl -fsS 'http://localhost:16686/api/v3/traces?$query' 2>/dev/null ||
        wget -qO- 'http://localhost:16686/api/v3/traces?$query'" >"$out/traces-$service.json" 2>/dev/null || true
    done
    { echo; node lab/profile/traces.mjs "$out/times" "$out"/traces-*.json; } >>"$out/summary.md"
  else
    echo -e "\nNo traces: tracing is off (lab/profile.sh on)." >>"$out/summary.md"
  fi
  echo "profile: $out/summary.md"
}

# symbolize names the frames of stripped Go binaries in profile $1's
# stacks, with go tool addr2line on a copy of each binary (lab/.cache/bin).
symbolize() {
  local out=$1 bin cache
  python3 -I lab/profile/symbolize.py list "$out"/*.folded >"$out/.unknown"
  : >"$out/.names"
  cut -d' ' -f1 "$out/.unknown" | sort -u >"$out/.bins"
  while read -r bin; do
    mkdir -p lab/.cache/bin
    # </dev/null: ssh would read the loop's list.
    cache=lab/.cache/bin/$(printf %s "$bin:$(on_node stat -c %s:%Y "$bin" </dev/null 2>/dev/null)" | shasum | cut -c1-12)-$(basename "$bin")
    if [ ! -s "$cache" ]; then
      on_node cat "$bin" >"$cache" </dev/null 2>/dev/null || { rm -f "$cache"; continue; }
    fi
    awk -v b="$bin" '$1 == b {print $2}' "$out/.unknown" >"$out/.addrs"
    # shellcheck disable=SC2094 # .addrs is only read
    "${GO_RUN[@]}" -i golang:1.27 go tool addr2line "$cache" <"$out/.addrs" | paste - - |
      paste "$out/.addrs" - | awk -v b="$bin" '{print b, $1, $2}' >>"$out/.names"
  done <"$out/.bins"
  python3 -I lab/profile/symbolize.py apply "$out/.names" "$out"/*.folded
  rm -f "$out/.unknown" "$out/.names" "$out/.addrs" "$out/.bins"
}

diff_profiles() {
  local a=$1 b=$2 out
  out=lab/profiles/diff-$(basename "$a")-vs-$(basename "$b")
  mkdir -p "$out"
  for k in oncpu offcpu; do
    [ -s "$a/$k.folded" ] && [ -s "$b/$k.folded" ] || continue
    # Red grew from A to B, blue shrank; scaled to A's total.
    flamegraph difffolded.pl -n "$a/$k.folded" "$b/$k.folded" |
      flamegraph flamegraph.pl --title "$k: $(basename "$a") → $(basename "$b")" >"$out/$k.svg"
  done
  echo "profile: $out"
}

ui() {
  if [ -n "$FERN_HOST" ]; then
    echo "Jaeger: http://localhost:16686 (Ctrl-C ends the tunnel)"
    exec "${SSH[@]}" -N -L 16686:127.0.0.1:16686 "root@$FERN_HOST"
  fi
  echo "Jaeger: http://localhost:16686 (Ctrl-C ends it)"
  exec docker exec -i "$LAB" kubectl -n fern-system port-forward deploy/jaeger 16686:16686 --address 0.0.0.0
}

case ${1:-} in
  on) lab_lock && on ;;
  off) lab_lock && off ;;
  run) shift && run "$@" ;;
  diff) diff_profiles "$2" "$3" ;;
  ui) ui ;;
  *) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//' >&2 && exit 2 ;;
esac
