#!/usr/bin/env bash
# Measures initialized wakes and sleeps on the VPS without an SSH or kubectl
# round trip inside the timed operation. Leaves its pf1- environment asleep.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
: "${FERN_HOST:?set FERN_HOST to the VPS; this benchmark does not use the local lab}"
env=${1:-pf1-wake} count=${2:-25}
[[ $env =~ ^pf1-[a-z0-9-]+$ && $count =~ ^[0-9]+$ && $count -ge 20 ]] || {
  echo 'usage: FERN_HOST=HOST lab/bench-wake.sh pf1-NAME COUNT (at least 20)' >&2
  exit 2
}

git archive HEAD:lab/sample-repo | on_node curl -sSf --unix-socket /run/fern/fern.sock \
  -X PUT -H 'Content-Type: application/x-tar' --data-binary @- "http://localhost/envs/$env"
on_node python3 - "$env" "$count" <<'PY'
import http.client
import json
import math
import socket
import subprocess
import sys
import time


class UnixConnection(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(180)
        self.sock.connect('/run/fern/fern.sock')


def request(path):
    conn = UnixConnection('localhost')
    started = time.perf_counter()
    conn.request('POST', '/envs/' + sys.argv[1] + path)
    response = conn.getresponse()
    body = response.read()
    conn.close()
    if response.status != 204:
        raise RuntimeError((response.status, body))
    return (time.perf_counter() - started) * 1000


def metrics():
    pod = subprocess.check_output([
        'kubectl', '-n', 'fern-system', 'get', 'pod',
        '-l', 'app.kubernetes.io/name=fern', '-o',
        'jsonpath={.items[0].metadata.name}',
    ], text=True)
    data = subprocess.check_output([
        'kubectl', 'get', '--raw',
        f'/api/v1/namespaces/fern-system/pods/{pod}:9090/proxy/metrics',
    ], text=True)
    return {line.split()[0]: float(line.split()[1])
            for line in data.splitlines()
            if line.startswith(('fern_wake_step_seconds_sum', 'fern_wake_step_seconds_count'))}


print(json.dumps({'warmup_ms': request('/wake')}), flush=True)
before = metrics()
rows = []
for cycle in range(int(sys.argv[2])):
    sleep_ms = request('/sleep')
    started = time.time()
    wake_ms = request('/wake')
    uid = subprocess.check_output([
        'kubectl', '-n', 'fern-boxes', 'get', 'pod', sys.argv[1],
        '-o', 'jsonpath={.metadata.uid}',
    ], text=True)
    row = {'cycle': cycle, 'sleep_ms': sleep_ms, 'wake_ms': wake_ms,
           'start_unix': started, 'pod_uid': uid}
    rows.append(row)
    print(json.dumps(row), flush=True)
after = metrics()
for step in ['sandbox', 'pod', 'ready']:
    key = f'fern_wake_step_seconds_count{{step="{step}"}}'
    if after[key] - before.get(key, 0) != len(rows):
        raise RuntimeError('another wake or a daemon restart changed the step metrics')
for name in ['sleep_ms', 'wake_ms']:
    values = sorted(row[name] for row in rows)
    print(json.dumps({'operation': name, 'n': len(values),
                      'p50': values[math.ceil(len(values) * .50) - 1],
                      'p95': values[math.ceil(len(values) * .95) - 1]}), flush=True)
print(json.dumps({'step_mean_ms': {
    step: 1000 * (after[f'fern_wake_step_seconds_sum{{step="{step}"}}'] -
                  before.get(f'fern_wake_step_seconds_sum{{step="{step}"}}', 0)) / len(rows)
    for step in ['sandbox', 'pod', 'ready']}}), flush=True)
request('/sleep')
PY
