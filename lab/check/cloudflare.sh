# shellcheck shell=bash
# Sourced by lab/check.sh, whose variables and helpers shellcheck can't see
# from here (SC2154, SC2034); the rest as there.
# shellcheck disable=SC2329,SC2086,SC2154,SC2034
# Section cloudflare of lab/check.sh, which sources this file: it has no use by itself.
# cloudflare: an Astro site on Cloudflare with D1 and R2 and no Fern files
# (lab/cloudflare-repo). Fern guesses its dev server, which emulates D1 and
# R2 in the workspace; a revert brings back a deleted row and object; Claude
# is told; and the node's Cloudflare token reaches the agent box's Claude,
# never the box that runs the repository's code.
cloudflare() {
  echo "== a Cloudflare repository: D1 and R2 in snapshots"
  local W=/var/lib/fern/worktrees/cloudflare-check G key secret=cf-check-token-$RANDOM saved=no
  G=$(golden_id cloudflare-check)
  for e in cf1 cf2 "$G"; do fern rm "$e" 2>/dev/null || true; done
  for wt in one two; do
    tar_dir lab/cloudflare-repo | in_daemon "rm -rf $W/$wt && mkdir -p $W/$wt && tar -x -C $W/$wt -f - && chown -R 1000:1000 $W/$wt"
  done
  # The first thread starts the golden build; the second starts from it.
  fern create cf1 --path $W/one >/dev/null
  start=$(now_ms)
  for _ in $(seq 300); do
    key=$(fern snapshots "$G" 2>/dev/null | awk '$1 ~ /^golden-/ { print $1 }')
    [ -n "$key" ] && break
    sleep 1
  done
  metric cloudflare.golden_build_ms "golden build of an Astro site on Cloudflare, ms" $(($(now_ms) - start))
  check "a golden snapshot is built from the guess" 1 "$(grep -c . <<<"$key")"
  if [ -z "$key" ]; then
    box_log "$G" | tail -20 | sed 's/^/  builder: /'
    return
  fi
  # Every pack on for the node, the node's own choice kept aside.
  local skills_saved=no
  if in_daemon "test -e /etc/fern/skills"; then
    in_daemon "cp -p /etc/fern/skills /etc/fern/skills.check" && skills_saved=yes
  fi
  on_node sh -c 'cat >/tmp/fern-install-skills.sh' <install.sh
  on_node bash /tmp/fern-install-skills.sh skills "beginner design engineering safety" >/dev/null
  in_daemon "rm /tmp/fern-install-skills.sh"
  fern create cf2 --path $W/two >/dev/null
  # api METHOD PATH [BODY] calls the site through the preview, which wakes
  # the box, and prints the body.
  api() { on_node curl -s -X "$1" -H "Host: cf2.localhost:30780" --data-raw "${3:-}" "http://127.0.0.1:30780$2" </dev/null; }
  start=$(now_ms)
  check "a preview wakes the thread and the guessed dev server serves" yes \
    "$(succeeds grep -q '<title>Scores</title>' <<<"$(api GET /)")"
  metric cloudflare.first_page_ms "asleep to the first page through the preview, from the golden, ms" $(($(now_ms) - start))
  api POST /api/scores '{"name":"ada","points":3}' >/dev/null
  api PUT /api/pic green >/dev/null
  check "D1 and R2 work, with no account" '[{"name":"ada","points":3}] green' "$(api GET /api/scores) $(api GET /api/pic)"
  check "their data is in the workspace" yes "$(succeeds in_daemon "test -d $W/two/.wrangler/state/v3/d1")"
  fern snapshot cf2 c1 >/dev/null
  api DELETE /api/scores >/dev/null
  api DELETE /api/pic >/dev/null
  check "deleted" "[] none" "$(api GET /api/scores) $(api GET /api/pic)"
  fern restore cf2 c1 >/dev/null
  check "a revert brings back the D1 row and the R2 object" '[{"name":"ada","points":3}] green' "$(api GET /api/scores) $(api GET /api/pic)"

  if [ "$AGENTS" = 1 ]; then
    # A Cloudflare token on the node, the real one kept aside.
    if in_daemon "test -e /etc/fern/cloudflare.env"; then
      in_daemon "cp -p /etc/fern/cloudflare.env /etc/fern/cloudflare.env.check" && saved=yes
    fi
    on_node sh -c 'cat >/tmp/fern-install-check.sh' <install.sh
    printf '%s\n' "$secret" | on_node bash /tmp/fern-install-check.sh cloudflare >/dev/null
    in_daemon "rm /tmp/fern-install-check.sh"
    # An agent is a program named claude: this one prints its arguments.
    in_daemon "mkdir -p $W/two/.fern/bin && printf '#!/bin/sh\nprintf \"%%s\\\\n\" \"\$@\"\n' >$W/two/.fern/bin/claude && chmod 755 $W/two/.fern/bin/claude && chown -R 1000:1000 $W/two/.fern/bin"
    check "Claude in the agent box gets Cloudflare's MCP server with the token" yes \
      "$(fern exec --agent cf2 -- $W/two/.fern/bin/claude -p | grep 'mcp.cloudflare.com' | grep -q "Bearer $secret" && echo yes || echo no)"
    # A pattern that doesn't match its own grep's command line.
    check "and, in an Astro site, Astro's docs server" yes \
      "$(fern exec --agent cf2 -- $W/two/.fern/bin/claude -p | grep -q 'mcp.docs.astro.build' && echo yes || echo no)"
    check "the box that runs the repository's code never sees it" no \
      "$(succeeds fern exec cf2 -- sh -c "grep -qs '${secret:0:1}[${secret:1:1}]${secret:2}' /proc/[0-9]*/environ /proc/[0-9]*/cmdline")"
    check "nor does the workspace" no "$(succeeds in_daemon "grep -rqs $secret $W /var/lib/fern/envs/cf2")"
    if [ $saved = yes ]; then
      in_daemon "mv /etc/fern/cloudflare.env.check /etc/fern/cloudflare.env"
    else
      in_daemon "rm -f /etc/fern/cloudflare.env"
    fi
  fi
  # Claude's box, the agent box if there is one, wrote the hint as it woke;
  # the skills are links into its image.
  local claude_box=""
  [ "$AGENTS" = 1 ] && claude_box=--agent
  check "Claude is told it is a Cloudflare repository" yes \
    "$(succeeds in_daemon "grep -q 'deploys to Cloudflare' /var/lib/fern/envs/cf2/claude/CLAUDE.md")"
  check "and gets Cloudflare's skills, fern-deploy and a skill of every pack the node turns on" "yes yes yes yes yes yes" \
    "$(fern exec $claude_box cf2 -- sh -c 'cd "$CLAUDE_CONFIG_DIR/skills" && for s in fern-deploy wrangler frameworks-on-cloudflare animate coding-standards site-safety; do test -f $s/SKILL.md && echo yes || echo no; done' | xargs)"
  check "and the beginner pack's note" yes \
    "$(succeeds in_daemon "grep -q \"isn't a developer\" /var/lib/fern/envs/cf2/claude/CLAUDE.md")"
  if [ $skills_saved = yes ]; then
    in_daemon "mv /etc/fern/skills.check /etc/fern/skills"
  else
    in_daemon "rm -f /etc/fern/skills"
  fi
  for e in cf1 cf2 "$G"; do fern rm "$e"; done
  in_daemon "rm -rf $W"
}
