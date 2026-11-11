---
name: fern-deploy
description: Put a Cloudflare project online from a Fern thread, with deploys on every push (main is the live site, other branches get preview URLs). Use when the person asks to deploy, publish, put the site online or share it, or to set up deploys, in a repository that deploys to Cloudflare (wrangler config or a Cloudflare adapter).
---

# Deploy from a Fern thread

In Fern, this box has no Cloudflare or GitHub credentials, by design. Deploys run on GitHub Actions, from the person's pushes, with a token Fern keeps on the server. Never run `wrangler deploy`, `wrangler login` or `cf login` here, and never ask the person for a token.

## Is it set up already?

If `.github/workflows/deploy.yml` is in the repository's history (`git log --oneline -1 -- .github/workflows/deploy.yml` prints a commit), deploys were set up before: skip to **Hand over**. Don't call `cloudflare_connect` again, and don't say the site can't go online because of it.

## Set it up, once per repository

1. **The workflow.** If `.github/workflows/deploy.yml` is missing, read this skill's `deploy.yml` (beside this file) and write it there. It installs with pnpm; for another package manager, change its setup and install steps to match the lockfile.
2. **The build installs cleanly.** With pnpm 10.34 or later, packages' install scripts run only if `pnpm-workspace.yaml` lists them under `onlyBuiltDependencies`: add `esbuild`, `workerd` and `sharp` if they are dependencies. Run the build (`pnpm build`) and fix what fails.
3. **The secret.** Call Fern's `cloudflare_connect` tool. It sets the repository's `CLOUDFLARE_API_TOKEN` secret on GitHub; you never see the token. If it says the server has no token or the GitHub App lacks a permission, tell the person what it says: they fix it once on the server.
4. **Production resources.** Bindings in `wrangler.jsonc` work locally without an account. For production, a D1 database needs its real `database_id`: create it with the `cloudflare` MCP server's tools if you have them, and write its ID in. R2 buckets and KV namespaces without an ID are created at the first deploy.
5. **Hand over.** You can't push from this box; the person does, in T3. End your answer with that one step, in plain words: "Commit and push this thread's changes in T3, and it goes online in about a minute." Say where it will be: `main` is the live site, `https://<name>.<subdomain>.workers.dev`; another branch gets its own preview, `https://<branch>-<name>.<subdomain>.workers.dev`, and the live site changes when the branch is merged. If you don't know the subdomain, say the run's summary on GitHub (Actions) shows the link.

## After that

Nothing to do per deploy: the person pushes. To see why a deploy failed, read the run on GitHub, or ask the `cloudflare` MCP server for the Worker's logs.
