# Shared helpers for the lab scripts. Source it; don't run it.
LAB=fern-lab
K3S_IMAGE=rancher/k3s:v1.36.5-k3s1
SANDBOX_VERSION=v1.0.7
LABEL=app.kubernetes.io/part-of=fern
LAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# k runs kubectl inside the lab.
k() { docker exec -i "$LAB" kubectl "$@"; }

# fern runs the fern CLI inside the daemon pod.
fern() { "$LAB_DIR/fern" "$@"; }

# in_daemon runs a shell command in the daemon container, which sees the pool.
in_daemon() { k -n fern-system exec -i ds/fern -c fern -- sh -c "$1"; }

# now_ms prints a wall-clock timestamp in milliseconds.
now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

# stats reads numbers, one per line, and prints "n=… p50=… p95=… max=…".
stats() {
  sort -n | awk '{a[NR]=$1} END {
    if (NR == 0) { print "n=0"; exit }
    p50=a[int((NR-1)*0.50)+1]; p95=a[int((NR-1)*0.95)+1]
    printf "n=%d p50=%s p95=%s max=%s\n", NR, p50, p95, a[NR] }'
}

FAILED=0
# check NAME EXPECTED ACTUAL records one pass/fail line.
check() {
  if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: want '$2', got '$3'"; FAILED=1; fi
}
