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
- [ ] **Pre-public cleanup (all mechanical, from code review P0/P1):**
  - de-panic proxy origin validation (`NewHandlers` returns error; delete duplicated
    `parseTrustedOrigin`, consume `config.ParseRemoteOrigin`) — H1;
  - replace 8× inline `"workspace manager is shutting down"` with one sentinel (`ErrClosing`);
  - add the Manager concurrency-invariant doc block (admissionMu→wakeMu order, lifecycle
    token, generation semantics);
  - move test-only `New`/`NewWithControls` into `_test.go`; drop the nil-waker branch;
  - `minDuration` → builtin `min`; dedupe backoff into one `nextBackoff`;
  - glossary comment: domain "pause" = docker stop, "Frozen" = freezer cgroup;
  - add minimal `.golangci.yml` (govet, staticcheck, errcheck, ineffassign) to CI.

## Weekend 2 — The number (1 day of code + half a day measuring)

- [ ] **Freezer pause behind `idle.mode: stop|freeze`** (classifier already understands frozen;
      extend `pauseObserved`/`resumeObserved`). Keep the two-pass idle barrier unchanged.
- [ ] **`fern wake --trace`** millisecond waterfall (proxy accept → thaw → health probe → first
      byte); run the lifecycle harness ×10, put the real numbers in the README.
- [ ] **Answer the one open review question** (the only decision owed): unhealthy-start →
      committed pause intent (docker.go rollback path) — deliberate anti-crash-loop or bug?
      Pick: distinct intent flavor *or* documenting comment + harness expectation.

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
