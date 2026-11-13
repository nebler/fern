# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section devserver of lab/check.sh, which sources this file: it has no use by itself.
# devserver starts Vite in the environment's box as Claude does, with MCP
# Bash's run_in_background, and checks that it keeps running after the call
# (a Bash call alone lets the box sleep 5 s later), then changes files from
# the agent box, as Claude's Edit and Write do: gVisor tells a box only of its
# own changes, so Fern passes them on (package watch). Vite is reached from
# the node, at the box's address: a command in the box would start the box's
# idle clocks and keep it awake by itself.
devserver() {
  echo "== a dev server Claude starts keeps running and hears Claude's edits"
  if [ "${AGENTS:-0}" != 1 ]; then
    note "skipped: no agent boxes (lab/boxes.sh agents)"
    return
  fi
  fern rm $ENV 2>/dev/null || true
  fern create $ENV >/dev/null
  # The box image has neither npm nor pnpm: the repository pins them, and
  # the box installs them when it wakes.
  fern exec $ENV -- sh -c 'printf "[tools]\nnode = \"22\"\npnpm = \"10\"\n" > mise.toml'
  fern sleep $ENV >/dev/null
  start=$(now_ms)
  check "a wake installs the tools mise.toml pins" v22 "$(fern exec $ENV -- node --version | cut -d. -f1)"
  note "a wake that installs Node 22 and pnpm 10, ms: $(($(now_ms) - start))"
  fern exec $ENV -- sh -c '
    set -e
    mkdir -p src
    echo "{}" > package.json
    pnpm add vite@7 >/dev/null 2>&1
    echo "<script type=module src=/src/main.js></script>" > index.html
    echo "export const v = 1" > src/main.js
    cat > vite.config.mjs <<EOF
import { appendFileSync } from "node:fs";
import { relative } from "node:path";
// Every event of the watcher Vite uses, in /tmp, where it is not watching.
export default {
  plugins: [{ name: "log", configureServer(s) {
    s.watcher.on("all", (e, p) => appendFileSync("/tmp/watch.log", e + " " + relative(process.cwd(), p) + "\\n"));
  } }],
};
EOF'
  # From here on only Claude's tools wake and use the box.
  fern sleep $ENV >/dev/null
  local job
  job=$(mcp $ENV Bash '{"command": "pnpm exec vite --host 0.0.0.0 --port 5173 --strictPort", "run_in_background": true}' |
    grep -o 'bash_[a-z0-9]*')
  check "Bash starts Vite in the background" yes "$([ -n "$job" ] && echo yes || echo no)"
  local ip
  ip=$(box_ip $ENV)
  # served PATTERN waits up to 10 s for Vite to serve src/main.js matching it.
  served() {
    # shellcheck disable=SC2016 # expanded on the node
    on_node sh -c 'for i in $(seq 100); do curl -sf "http://$1:5173/src/main.js" | grep -q "$2" && exit; sleep 0.1; done; exit 1' sh "$ip" "$1" </dev/null
  }
  if ! served 'v = 1' >/dev/null 2>&1; then
    check "Vite serves src/main.js" yes no
    mcp $ENV BashOutput "{\"bash_id\": \"$job\"}" | sed 's/^/  vite: /'
    return
  fi
  sleep 65
  check "a minute after the Bash call, the box is awake" running "$(state $ENV)"
  check "… and Vite still serves" yes "$(succeeds served 'v = 1')"
  check "BashOutput shows Vite's output" yes "$(succeeds grep -q 'ready in' <<<"$(mcp $ENV BashOutput "{\"bash_id\": \"$job\"}")")"
  restart_daemon
  check "after the daemon restarts, Vite still serves" yes "$(succeeds served 'v = 1')"
  check "… and BashOutput still reaches it" yes "$(succeeds grep -q 'is running' <<<"$(mcp $ENV BashOutput "{\"bash_id\": \"$job\"}")")"

  start=$(now_ms)
  fern exec --agent $ENV -- sh -c '
    echo "export const v = 2" > src/main.js
    echo "export const n = 1" > src/new.js
    mkdir -p src/feature/deep && echo "export const d = 1" > src/feature/deep/d.js'
  check "after Claude's edit, Vite serves the new src/main.js" yes "$(succeeds served 'v = 2')"
  note "edit to served: $(($(now_ms) - start)) ms, the agent box's exec included"
  # heard EVENT waits up to 5 s for Vite's watcher to report it.
  heard() {
    local out
    out=$(mcp $ENV Bash "{\"command\": \"for i in \$(seq 50); do if grep -Eqx '$1' /tmp/watch.log; then exit 0; fi; sleep 0.1; done; exit 1\"}") || return
    ! grep -q 'exit code' <<<"$out"
  }
  check "Vite's watcher hears the edit" yes "$(succeeds heard 'change src/main.js')"
  check "… a new file" yes "$(succeeds heard '(add|change) src/new.js')"
  check "… a file in new folders" yes "$(succeeds heard '(add|change) src/feature/deep/d.js')"
  note "Vite's watcher heard: $(mcp $ENV Bash '{"command": "sort -u /tmp/watch.log | tr \"\\n\" \";\""}')"

  check "KillShell stops Vite" yes "$(succeeds grep -q 'is killed' <<<"$(mcp $ENV KillShell "{\"shell_id\": \"$job\"}")")"
  check "… and then the box sleeps after the last Bash call's 5 s" asleep "$(asleep_within $ENV 20)"

  # Previews, as a browser on the machine asks for them. A preview request
  # wakes the box and counts as its use, so these come last.
  # preview HOST PATH prints what the front door answers for HOST.
  preview() {
    on_node curl -s -H "Host: $1" "http://127.0.0.1:30780$2" </dev/null
  }
  check "a preview of a port nothing listens on says so" yes "$(succeeds grep -q 'nothing listens on port 5173' <<<"$(preview "5173-$ENV.localhost:30780" /)")"
  # A dev server in services.yaml starts with the box, which a preview wakes.
  fern exec $ENV -- sh -c 'printf "services:\n  web:\n    run: pnpm exec vite --host 0.0.0.0 --port \$PORT --strictPort\n    port: 5173\n" > services.yaml'
  fern sleep $ENV >/dev/null
  start=$(now_ms)
  check "the default preview http://$ENV.localhost:30780/ wakes the box and serves Vite" yes "$(succeeds grep -q 'v = 2' <<<"$(preview "$ENV.localhost:30780" /src/main.js)")"
  note "asleep to served through the preview: $(($(now_ms) - start)) ms"
  check "… as does http://5173-$ENV.localhost:30780/" yes "$(succeeds grep -q 'v = 2' <<<"$(preview "5173-$ENV.localhost:30780" /src/main.js)")"
  check "… with its live reload's client" yes "$(succeeds grep -q WebSocket <<<"$(preview "$ENV.localhost:30780" /@vite/client)")"
  fern exec --agent $ENV -- sh -c 'echo "export const v = 3" > src/main.js'
  check "an edit of Claude's reaches the preview" yes "$(succeeds on_node sh -c 'for i in $(seq 100); do curl -s -H "Host: $1" http://127.0.0.1:30780/src/main.js | grep -q "v = 3" && exit; sleep 0.1; done; exit 1' sh "$ENV.localhost:30780" </dev/null)"
}
