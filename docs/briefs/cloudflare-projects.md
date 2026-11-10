# Cloudflare projects on Fern

Status: steps 2 to 5 built 2026-11-10 (`lab/check.sh cloudflare`); step 1 needs T3's app, which Fern doesn't ship.

## Why
A small site on Astro and Cloudflare (Workers, D1 for data, R2 for pictures) is the kind of project a person with a slow laptop builds through T3 on a Fern server. The goal: a Cloudflare repository runs, reverts and deploys from a thread with nothing to set.

## Tried (VPS, 2026-11-10, `nebler/nutri-games`)
- Astro 7 with `@astrojs/cloudflare` 14 and wrangler 4 build in a box (`astro build` 2 s), and `wrangler dev` (workerd) serves the page and an API route under gVisor.
- With `mise.toml` (Node 22, pnpm 10) and a `services.yaml` running `astro dev`: golden build 72 s, then a page through the preview in 0.17 s.
- A first try hung at wake: its `package.json` had a `dev` script and no dependencies yet, so the guessed `pnpm run dev` had no `astro` to run. Not a bug in the guess, which installs before it starts the dev server.
- pnpm 10.34 no longer reads `pnpm.onlyBuiltDependencies` from `package.json`; it goes in `pnpm-workspace.yaml`, or workerd's and esbuild's install scripts don't run.
- A project T3 cloned from GitHub lands in T3's home (`/home/node/<repo>`), outside the pool. A Local thread there ran Claude in T3's own service, unboxed: `fern-claude` refused sessions only in `repos/` and `projects/`. Fixed: it refuses a session anywhere that isn't a Fern workspace (`lab/check.sh t3`).

## Built
1. **Detecting a Cloudflare project** (`setup.IsCloudflare`): `wrangler.jsonc`, `wrangler.json` or `wrangler.toml` at the root, or a dependency on `wrangler`, `@astrojs/cloudflare`, `@cloudflare/vite-plugin` or `@opennextjs/cloudflare`. A Worker with no framework (`"dev": "wrangler dev"`) gets wrangler's flags and port 8787 from the guess; a framework on Cloudflare keeps its own dev server, which runs workerd.
2. **Claude is told** (`fern-init`'s hint): D1, R2 and KV are emulated in `.wrangler/state`, with no account; that data is snapshotted with every turn; deploys happen by pushing from T3; no `wrangler deploy` or `wrangler login` in the box.
3. **Local Cloudflare state is the environment's.** `.wrangler/state` is in the workspace, so a turn's snapshot takes it and a revert brings it back, as with Postgres. `lab/check.sh cloudflare` writes a D1 row and an R2 object through the preview, snapshots, deletes them, restores, and reads them back.
4. **Deploys by push.** Cloudflare's Workers Builds deploys a push to `main` and gives other branches a preview URL; T3's threads push through the GitHub App. Fern adds no code here, only the hint's line and the README.
5. **Opt-in Cloudflare MCP for the agent box.** `FERN_CLOUDFLARE_TOKEN_FILE=… install.sh root@HOST` (or `install.sh cloudflare` on the server, the token on stdin) keeps the token in `/etc/fern/cloudflare.env`, root only. The daemon reads it at each agent's start, and only for a repository that deploys to Cloudflare adds Cloudflare's MCP server (`https://mcp.cloudflare.com/mcp`, the token as its bearer token) to Claude's `--mcp-config`, or OpenCode's settings, in the agent box. The box that runs the repository's code, the workspace, goldens and snapshots never get it (`lab/check.sh cloudflare` greps for it). `fern doctor` says whether it is set.

## Next: onboarding with one click (proposed 2026-11-10)
Goal: a person's only steps are the Cloudflare and GitHub logins no one can take for them. Tried by hand on `nutri-games` first.
1. **Deploy from GitHub Actions, not Cloudflare's GitHub app.** Cloudflare's Git integration needs its GitHub app and dashboard clicks; a workflow (`wrangler deploy` for `main`, `wrangler versions upload --preview-alias <branch>` for other branches) needs only a file and a repository secret, both of which can be scripted.
2. **One token, from a pre-filled link.** Cloudflare's token form takes `permissionGroupKeys` in its URL: Workers Scripts, D1, R2 and KV edit, Account Settings read. `install.sh` would print the link and ask for the token, as it does for Claude's login.
3. **Fern does the rest per repository.** When a Cloudflare repository's golden is built and the node has a token, the daemon sets the repository's `CLOUDFLARE_API_TOKEN` secret through the GitHub App (it needs *Secrets: write*; the value is sealed with the repository's public key, `golang.org/x/crypto/nacl/box`), and Claude's hint offers Fern's workflow when the repository has none (the App needs *Workflows: write* to push it). Done when a new Cloudflare repository deploys on its first push with nothing set by hand.

## Not built: hiding Local
The Local / New worktree picker is in T3's app, not its server: the app chooses the mode (`resolveNewThreadEnvMode`, `packages/shared/src/projectSettings.ts`) from the server's `defaultThreadEnvMode`, which Fern sets to `worktree`, and the project's own default, which is unset. A server patch can't hide the choice. What Fern has: the default, and `fern-claude` refusing the session with a message naming New worktree. The fix belongs upstream: a server setting T3's app honours, such as "worktrees only", that removes Local from the picker.

## Open
- Whether Cloudflare's GitHub app can be connected without a browser (the repo-connection API exists; installing the app needs one).
- An MCP login per person once a server has several (roadmap: Not now).
- With agent boxes, the hint the agent box writes (last) names no services, as that box runs none. Seen while building this; not changed.
