# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section notify of lab/check.sh, which sources this file: it has no use by itself.
# notify gives the daemon a URL to tell (--notify-url) and a receiver on the
# node that keeps what is POSTed to it, breaks a golden build, and checks
# that the failure is told once, not again by a restarted daemon, and that
# the daemon says so when nothing is wrong any more. The daemon looks once a
# minute, so this takes four.
notify() {
  echo "== a failure is told to a URL, once"
  local W=/var/lib/fern/worktrees/ck-notify G args got=/tmp/fern-notified
  G=$(golden_id ck-notify)
  args=$(daemon_args)
  for e in $ENV ${ENV}2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  in_daemon "rm -f $got /var/lib/fern/notified.json; pkill -f fern-notify-receiver || true"
  # The receiver appends each POST's body, on a line of its own.
  on_node sh -c "nohup python3 -c '
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get(\"Content-Length\", 0)))
        open(\"$got\", \"ab\").write(body.replace(b\"\\n\", b\" \") + b\"\\n\")
        self.send_response(200); self.end_headers()
    def do_GET(self):
        open(\"$got\", \"ab\").write(b\"beat\\n\")
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer((\"127.0.0.1\", 8099), H).serve_forever()  # fern-notify-receiver
' >/dev/null 2>&1 &" </dev/null
  # shellcheck disable=SC2086 # the daemon's arguments are words
  set_daemon_args $args --notify-url=http://127.0.0.1:8099/fern
  restart_daemon
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/one && mkdir -p $W/one/.fern && tar -x -C $W/one -f - &&
    printf '%s\n' '#!/bin/sh' 'echo no such tool: frobnicate >&2' 'exit 3' > $W/one/.fern/setup &&
    chmod +x $W/one/.fern/setup && chown -R 1000:1000 $W/one"
  fern create $ENV --path $W/one >/dev/null
  for _ in $(seq 150); do
    in_daemon "grep -q 'golden build' $got" 2>/dev/null && break
    sleep 1
  done
  check "a failed golden build is POSTed to the URL, with the builder's name" 1 "$(in_daemon "grep -c 'golden build.*$G' $got" || true)"
  restart_daemon
  sleep 75
  check "a restarted daemon doesn't tell it again" 1 "$(in_daemon "grep -c 'golden build' $got" || true)"
  # Mended, the next worktree's build works, and the last problem is gone.
  in_daemon "printf '%s\n' '#!/bin/sh' 'true' > $W/one/.fern/setup"
  tar_dir lab/sample-repo | in_daemon "rm -rf $W/two && mkdir -p $W/two/.fern && tar -x -C $W/two -f - &&
    cp -p $W/one/.fern/setup $W/two/.fern/setup && chown -R 1000:1000 $W/two"
  fern create ${ENV}2 --path $W/two >/dev/null
  for _ in $(seq 200); do
    in_daemon "grep -q 'nothing is wrong any more' $got" 2>/dev/null && break
    sleep 1
  done
  check "and says when nothing is wrong any more" 1 "$(in_daemon "grep -c 'nothing is wrong any more' $got" || true)"
  # A sign of life every --heartbeat-every, for a service that alerts when
  # they stop; and a unit that fails for good says so (OnFailure=).
  in_daemon "rm -f $got"
  # shellcheck disable=SC2086
  set_daemon_args $args --heartbeat-url=http://127.0.0.1:8099/beat --heartbeat-every=2s
  restart_daemon
  sleep 7
  check "the daemon sends a sign of life every 2 s" yes "$([ "$(in_daemon "grep -c '' $got 2>/dev/null" || echo 0)" -ge 3 ] && echo yes || echo no)"
  in_daemon "echo FERN_NOTIFY_URL=http://127.0.0.1:8099/fern > /etc/fern/notify.env && chmod 600 /etc/fern/notify.env && rm -f $got"
  on_node systemctl start fern-failed@fern-daemon.service.service </dev/null || true
  check "a unit that failed for good is told to the URL" 1 "$(in_daemon "grep -c 'fern-daemon.service failed' $got" || true)"
  in_daemon "rm -f /etc/fern/notify.env"
  # shellcheck disable=SC2086
  set_daemon_args $args
  restart_daemon
  in_daemon "pkill -f fern-notify-receiver || true; rm -f $got"
  for e in $ENV ${ENV}2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  in_daemon "rm -rf $W"
}
