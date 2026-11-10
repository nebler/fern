---
name: site-safety
description: Make a small public website safe before and after it goes online - forms, uploads, stored scores or comments, logins, keys and visitors' data. Use before the first deploy, when adding anything that accepts input from visitors, or when asked whether the site is safe.
---

# Site safety

A small site gets the same bots and abuse as a big one. Check each of these where it applies, fix what fails, and tell the person in plain words what you protected.

## Keys and secrets
- No API key, token or password in code that reaches the browser, in the repository, or in `wrangler.jsonc`'s `vars`. Server-side secrets go in Cloudflare secrets (`wrangler secret put`, by the person or a deploy, never from this box) and are read from `env` in the Worker.
- Nothing secret in client-side environment variables (`PUBLIC_*`, `VITE_*`): those are published.

## What visitors send
- **Check everything on the server:** types, lengths, ranges (a score is a whole number within what the game allows). The browser's checks are only for convenience.
- **SQL:** always bound parameters (`prepare("... ?").bind(x)`), never text joined into a query.
- **Output:** let the framework escape what visitors wrote; never put it into HTML with `set:html`, `innerHTML` or `dangerouslySetInnerHTML`.
- **Uploads (R2):** limit the size (a few MB) and the types (check the file's first bytes, not its name), store under names you generate, and serve with the right `Content-Type`. Pictures from visitors are public: say so.

## Bots and abuse
- Forms, sign-ups, score submissions and uploads get **Turnstile** (the turnstile-spin skill, or Cloudflare's docs), checked on the server.
- **Rate limits** on anything that writes: Workers' rate limiting binding, or a counter per visitor in KV or D1.
- Scores can be faked from the browser: if a leaderboard matters, check on the server that a score is possible, or say that it can be cheated.

## Visitors' data
- Collect as little as you can. No personal data from children; avoid names, emails or photos of people unless the site needs them, and then say what is kept and why.
- No tracking scripts or third-party embeds without the person agreeing.

## Before the first deploy
Run Claude Code's `/security-review` over the site's code, fix what it finds, and summarize the result for the person in a few lines.
