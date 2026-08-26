# fern: Next Actions — Pre-Decided, No Check-Ins Required

Synthesized from R5–R7 and the code review. Every item here had its decision already made by
research; execution only. Guard rail at the bottom lists what is deliberately *not* on this
list.

## Evenings this week (zero build time)

- [ ] **Send 5 referral-outreach DMs.** Targets: Grab Applied AI engineers publicly writing on
      the gateway — Kendrick Tan, Jeffery Lean, Yi Sheng Tai (Agent Platform Part 1 authors),
      plus any 2nd-degree connections. Personalize with one specific line from their post.
      Referrals are the highest-leverage channel found; costs nothing.
- [ ] **Rewrite CV with the three fern bullets** drafted in `product-docs/r7.md` §8.
- [ ] **Draft four STAR stories mapped to Heart / Hunger / Honour / Humility** (one page
      total). Culture round is official and HM-led; zero prep exists today.
- [ ] **Subscribe to engineering.grab.com / check weekly.** Agent Platform **Part 2** (gateway
      deep-dive) is unpublished; reading it within days of release is free differentiation in
      interviews.

## Weekend 1 — Go public (1–2 days)

- [ ] **Make the repo public.**
- [ ] **README rewrite for the 5-minute reviewer**, in this order: Why/problem → demo GIF →
      two headline numbers (placeholders OK) → architecture diagram → boundary docs linked
      below. Current README is a correctness contract; reviewers get 90 seconds.
- [ ] **One architecture diagram**: phone → Tailscale Serve → remote listener → admission gate
      → wake → Docker workspace (`opencode serve`), plus the task pipeline line
      (admit → exactly-once delivery → seal → verify → draft PR).
- [ ] **Record a 60–90s demo**: phone on cellular → scan QR → submit task → watch wake → PR
      link. GIF at top of README beats linked video.
- [x] **Pre-public cleanup — DONE 2026-08-23** (commits `69eba67`, `a31288f`, `5a8308f`,
      `a6fcfc0`, `0a90066`): origin validation returns errors instead of panicking; legacy
      constructors relocated behind tests; nil-waker path documented; `ErrManagerClosed`
      sentinel replaces eight inline literals; Manager lock-ordering contract documented;
      gh-exec failures keep wrapped causes; builtin `min`; glossary comment for domain
      "pause" vs Docker "Frozen"; `.golangci.yml` + `make lint` added.
      *(Backoff dedup finding dissolved on verification — code already shared one helper.)*

## Weekend 2 — The number (1 day of code + half a day measuring)

- [x] **`fern debug wake` shipped 2026-08-23** — per-phase waterfall served through the
      operator listener (`POST /fern/api/v1/debug/wake-trace`): span plumbing in runtime,
      trace recording on each coalesced wake, operator-only endpoint + CLI verb.
- [x] **Freezer pause implemented 2026-08-24** — `idle.mode: stop|freeze` config
      (+ `-idle-mode` flag), freeze branch in `pauseObserved` with its own
      failure reconciler, intent journal retained in both modes (reboot-safe
      classification), unit-tested against a mocked Docker API.
- [ ] **Run `fern debug wake` ×10 through the lifecycle harness** with
      `idle.mode: freeze`; put the real before/after numbers in the README table.
- [x] **Classify unhealthy starts distinctly** — failed health/observer startup
      rollback commits a `failedStart` intent, so a never-healthy container remains
      `failed` rather than appearing intentionally paused.

## Weekend 3 — Apply + gateway skeleton

- [ ] **Apply Sunday night** to the PJ req (744000137791699) + sibling Backend-AI req. Do not
      wait for "finished"; the ~1-month loop timeline means v1 lands before anyone probes it,
      and the req can close without notice.
- [ ] **Gateway skeleton** (scope locked, no expansion): host-side key custody + hashed scoped
      tokens; `/chat/completions` + `/v1/messages` passthrough; SSE hardened — unconditional
      `X-Accel-Buffering: no` + `Cache-Control: no-cache`, `[DONE]` framing, inject
      `stream_options.include_usage`, Anthropic cumulative-usage handling, TTFT stamped
      first-byte/write-once. Embed, don't invent: `go-redis/redis_rate` comes next weekend,
      pricing from pinned LiteLLM JSON after that.

## Standing (weekly cadence)

- [ ] DSA 45–60 min/day (Codility-style timed, LC easy-medium) + one system-design rehearsal
      per week using **fern as the worked example** (rate limiter → API gateway → streaming).
- [ ] One mock interview per week from week 4 onward.
- [ ] Prep the PJ-specific round type: SQL query + code-review discussion (reported by the one
      accepted PJ candidate).

## Deliberately NOT on this list (don't relitigate)

Terminal-success observer before the gateway · benchmark essay · Kubernetes/microVMs ·
native mobile app · multi-workspace · multi-harness support · any new strategy document.

After weekend 3, the split flips to ~30% fern / 70% interview prep until the loop forces a
change. Full reasoning: `product-docs/r7.md`.
