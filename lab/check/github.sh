# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section github of lab/check.sh, which sources this file: it has no use by itself.
github() {
  echo "== T3's git and gh get tokens from the daemon's GitHub App"
  local args dir=/tmp/fern-github-check
  args=$(daemon_args)
  in_daemon "set -e; rm -rf $dir; mkdir -p $dir/app; echo 7 >$dir/app/id
    openssl genrsa -traditional -out $dir/app/key.pem 2048 2>/dev/null
    cat >$dir/fake.py <<'PY'
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def reply(self, code, body):
        self.send_response(code); self.send_header('Content-Type', 'application/json'); self.end_headers()
        self.wfile.write(json.dumps(body).encode())
    def do_GET(self):
        if not self.headers.get('Authorization', '').startswith('Bearer ey'):
            self.reply(401, {'message': 'Bad credentials'})
        elif self.path == '/repos/owner/app/installation':
            self.reply(200, {'id': 42})
        elif self.path.startswith('/app/installations?'):
            self.reply(200, [{'id': 42, 'account': {'login': 'owner'}, 'permissions': {'contents': 'write', 'pull_requests': 'write'}}])
        else:
            self.reply(404, {'message': 'Not Found'})
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path != '/app/installations/42/access_tokens':
            self.reply(400, {})
        elif body.get('repositories') == ['app']:
            self.reply(201, {'token': 'ghs_check', 'expires_at': '2099-01-01T00:00:00Z'})
        elif 'repositories' not in body:
            self.reply(201, {'token': 'ghs_account', 'expires_at': '2099-01-01T00:00:00Z'})
        else:
            self.reply(400, {})
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', 18080), H).serve_forever()
PY
    (python3 $dir/fake.py >/dev/null 2>&1 &) ; sleep 0.5"
  set_daemon_args $args --github-app=$dir/app --github-api=http://127.0.0.1:18080
  local ask='printf "protocol=https\nhost=github.com\npath=owner/app.git\n\n" | git credential fill'
  check "T3's git gets a token for a repository with the App" password=ghs_check \
    "$(in_t3 "$ask" 2>&1 | grep '^password=')"
  check "and none for one without it" "" \
    "$(in_t3 "${ask/owner\/app/owner/other} </dev/null 2>/dev/null; true" 2>&1 | grep '^password=' || true)"
  check "T3's gh gets one through its wrapper" ghs_check "$(in_t3 'fern github-token owner/app' 2>&1)"
  check "T3's image has GitHub's CLI" yes "$(succeeds in_t3 'gh --version >/dev/null')"
  # T3 asks for its GitHub credential this way, in its own folder, once for
  # the host, and uses it for every thread's pull requests.
  check "T3's own question for a credential gets the account's token" ghs_account \
    "$(in_t3 'cd /opt/t3 && gh auth token --hostname github.com' 2>&1)"
  set_daemon_args $args
  in_daemon "pkill -f $dir/fake.py; rm -rf $dir"
}
