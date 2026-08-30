# Security Boundary

## Status

This document records the current phone, operator-control, OpenCode, and private
edge trust boundary plus the work required for a supported release. Code and
tests remain authoritative for implemented behavior. `GITHUB_INTEGRATION.md`
owns publication and GitHub App details.

Fern currently assumes one trusted owner, host, repository, image, Docker
daemon, and tailnet. It is not a hostile multi-tenant sandbox. Tailscale provides
private reachability, not an application-level user identity. OpenCode and
repository code execute inside the trusted workspace boundary and may access
provider credentials intentionally exposed there.

## Current Controls

- Fern binds only to numeric loopback and requires a private TLS edge for phone
  access.
- Tailscale Funnel is outside the supported deployment.
- Pairing uses a 256-bit five-minute capability. GET only previews; POST
  consumes the code and creates a digest-backed 30-day device grant.
- Pairing codes, issuance, successful exchanges, per-code attempts, and global
  failures are bounded; limiter state and durable device grants survive restart.
- Device cookies are named `__Host-fern_device`, are `Secure`, `HttpOnly`,
  `SameSite=Strict`, path `/`, and domainless, and are stripped before
  proxying to OpenCode. Upstream responses cannot set either the current or
  reserved future Fern device-cookie name; unrelated `Set-Cookie` headers are
  left untouched.
- Paired devices can use OpenCode and the durable task API, including eligible
  receipt-backed App publication. They cannot access operator control or retired
  host-credential publication routes.
- Fern control credentials and common GitHub token variables are rejected from
  workspace configuration by key, by `${...}` source reference under an alias,
  and by post-expansion equality with the control credential.
- The legacy host-credential publication package is not composed by `fern up`;
  effecting routes reject use and the standalone command is dry-run only.
  Historical unresolved records block readiness until an operator stops Fern,
  inspects them, and runs `fern debug quarantine-publications`; quarantine marks
  them atomically and never replays an effect.
- Fern-owned HTML uses a restrictive CSP and control mutations perform an
  Origin check against the canonical operator origin, not client `Host`.
  Fetch Metadata rejects same-site siblings and cross-site browser mutations.
  Device-cookie mutations additionally require short-lived HMAC tokens bound to
  credential, method, and route; explicit Basic-auth mutations are token-exempt.
- The GitHub App setup route is operator-authenticated on loopback. Only its
  exact callback route is reachable without a device grant on remote ingress;
  callback authorization is a bounded random one-use state, authority must equal
  the configured HTTPS origin, and ambient cookies/Authorization are stripped.
  Pending callbacks survive Fern restart through a digest-only state lookup;
  claimed callbacks never regain exchange authority after a crash.
- App-bound task mode never initializes the legacy host-`gh` publisher.
  Repository-scoped installation tokens are delivered to Git through a
  single-use private askpass file and are absent from argv, durable records,
  returned evidence, and logs. The publication coordinator pauses workspace
  compute, revalidates the exact durable selection under that retained fence,
  and keeps it through every Git/GitHub call and store transition.
- App publication admission accepts only an exact sealed result and successful
  verification ID. One transaction derives the complete immutable tuple and
  stores its actor, event, and `result.publish` receipt. Schema-6 workers cannot
  discover or update migrated publication rows without such a receipt.
- Verification commands come only from strict host configuration. Fern inserts
  no shell, rejects scripts, resolves native executables with symlink-refusing
  `openat` traversal, rejects unsafe writable parents/files and executables
  inside the writable workspace, and binds metadata plus content SHA-256 into
  durable policy/runner identity. Linux executes pinned descriptors; Darwin
  executes private descriptor-sourced copies and rejects Apple's relocatability-
  incompatible Git shim. Every invocation tears down its process group before
  postflight and bounds output draining. The runner uses a non-ambient bounded
  environment and proves the exact clean commit before and after execution while
  workspace compute remains paused under a retained manager fence. A malicious
  process can escape a process group with a new session, so repository code and
  operator-supplied environment remain in the trusted single-owner host
  boundary; this is not a hostile-code sandbox.
- Optional `proxy.remoteOrigin` is a strict canonical HTTPS root. On both
  ingresses Fern removes `Forwarded` and all `X-Forwarded-*` input, sets the
  upstream host/scheme/effective port from trusted configuration, and never
  generates `X-Forwarded-For`.
- Offline credential export uses age X25519 encryption. Import and rotation
  require absent compute and the workspace lease, validate the exact configured
  GitHub identity and permissions live, and write an encrypted prior-generation
  rollback before replacement. Import can bootstrap an absent generation
  without a rollback artifact and restores absence on activation failure;
  rotation requires a prior generation. Active credential stores remain
  permission-protected plaintext, and GitHub revocation remains external.
- Offline backup creation destroys compute, checkpoints SQLite, exports the
  exact managed volumes, verifies checksums, and separates detected credentials
  and opaque volumes. Restore creates a durable pre-restore operational rollback
  generation before sequential filesystem and volume activation.

These controls are useful but do not complete the product boundary below.

Workspace-`gh` intentionally makes the stored credential available to trusted
workspace code and may carry account-wide authority. Direct Git/`gh` mutations
have no Fern receipt or ambiguity reconciliation. This is not equivalent to a
repository-scoped GitHub App installation token.

## Residual Findings

### Complete Browser-Safe Operator Separation

Production now has two policy-separated listeners. The Tailscale-facing remote
listener rejects operator and backend Basic credentials and unconditionally
denies control routes, so paired/OpenCode-origin JavaScript cannot reach operator
controls through that origin. The second listener remains loopback-only and is
the sole surface for local CLI attach and operator control.

The operator listener still carries the OpenCode protocol needed by the local
CLI. Loading the official OpenCode browser UI on that listener after entering
operator Basic would recreate a same-origin confused-deputy risk. Treat it as a
host CLI/control surface, not a supported OpenCode browser origin. A supported
operator browser workflow still needs a control-only third logical surface or
CLI-only controls, with an explicit token contract if browser support is added.

### Revoke Established Connections

Device authentication now returns durable identity and registers every admitted
request context under that device. Successful persisted revocation cancels all
registered requests for the target device before returning, without affecting
other devices, and reconnect is denied. Unit/race coverage includes ordinary
requests, uploads, SSE-like streams, and upgrade-shaped requests. The standard
reverse proxy closes upgraded connections abruptly on context cancellation
rather than sending a WebSocket close frame; the real OpenCode PTY transport and
physical phone still require acceptance coverage. Active request contexts also
inherit the durable grant expiry, so a stream cannot outlive its device grant.

### Backend And Remote Credentials Are Split

The password known inside the OpenCode container is accepted only on the
loopback operator surface for local CLI compatibility. Remote ingress rejects
it before wake and accepts selective device grants instead. Fern strips incoming
Authorization and regenerates canonical backend Basic after successful
admission. Browser/lifecycle/real-image harnesses cover this negative boundary.

### Provider Credentials Still Enter The Trusted Workspace

`ANTHROPIC_API_KEY` and `OPENAI_API_KEY` may be forwarded into the OpenCode
container. That is consistent with the current single-owner trusted-repository
model, but it means agent tools and repository code can read and exfiltrate the
provider credential. Fern does not currently provide proxy-only provider
credentials, model-scoped tokens, model allowlists, request budgets, or a model
audit ledger.

Any future Fern Gateway would move provider custody to the host and give the
workspace a scoped Fern credential. Until that is implemented and accepted,
documentation must not claim Palana-style proxy-only secrets or workload
identity. See [Fern Roadmap](./ROADMAP.md).

### Prove Backend Authentication Fails Closed

Fern startup previously performed only a positive authenticated health check,
which could not distinguish a protected backend from one that ignored
authentication. Startup now requires missing and intentionally incorrect
credentials to receive `401` before accepting correctly authenticated health.
The selected pinned OpenCode image and environment-variable contract still must
retain this proof in its black-box release gate.

## Additional Required Hardening

1. Add per-request CSRF tokens to operator HTML forms and decide which
   non-browser mutation clients may omit `Origin`/Fetch Metadata. Exact trusted
   origins and cross-site Fetch Metadata rejection are already enforced.
2. Treat tailnet ACLs and grants as a reviewed deployment gate. Do not infer a
   person from device name, source IP, or Git author.
3. Add bounded issuance rate policy in addition to the outstanding pairing-code
   cap when the product supports more than one trusted operator.
4. Require external revocation and verification of superseded App keys or
   workspace OAuth tokens after local rotation; Fern cannot perform that GitHub
   account action.
5. Physically characterize abrupt power loss during sequential restore across
   filesystem roots and Docker volumes. The local rollback path is not a claim
   of cross-domain atomicity.

## Parallel Security Tracks

| Track | Can start | Merge gate |
| --- | --- | --- |
| Remote confused-deputy boundary (implemented) | Device-only ingress rejects Basic and controls before wake | Browser-safe operator control still requires a control-only surface or CLI-only contract |
| Backend auth negative probes (implemented) | Startup and exact-image smoke require missing/wrong auth rejection | Keep as release regression gate |
| Device connection attribution/expiry (implemented) | Durable identity, race-safe cancellation, and expiry deadlines are covered | Real OpenCode SSE/PTY and physical-phone revocation acceptance remain |
| Operator listener (implemented, host-only) | Local attach/control is separate from Serve ingress | Must not be published or used as a supported OpenCode browser origin |
| Canonical HTTPS forwarding (implemented) | Fake edge proves spoof stripping and absolute HTTPS metadata | Pinned-image real TLS/WSS and OAuth acceptance still block correctness claims |
| CSRF/Fetch Metadata hardening (partial) | Exact origin and Fetch Metadata rejection implemented; form tokens remain | Blocks browser control mutations |
| Tailnet ACL-negative rehearsal | Requires an explicit second tailnet principal | External release gate |

## Security Acceptance

A supported release must prove:

1. OpenCode-origin JavaScript cannot invoke operator controls, even after an
   operator used the same device.
2. Revoking a device immediately closes its established streams and terminals
   and rejects reconnect. Unit/race coverage exists; real OpenCode and phone
   transport evidence remains required.
3. The backend credential present in Docker is rejected at remote ingress.
4. Fern refuses to publish an OpenCode backend that accepts missing or wrong
   credentials. This is covered by runtime unit tests and must remain in the
   exact-image smoke gate.
5. Forwarded scheme and host produce correct HTTPS links, redirects, SSE, and
   WebSocket behavior without trusting spoofed headers.
6. Every browser mutation has an attributable actor, exact-origin protection,
   CSRF defense, bounded input, durable intent, and an audit event.
7. A tailnet principal outside the reviewed ACL cannot reach the Fern origin.

No checked-in evidence proves the physical phone, private TLS/WSS, reboot,
replacement-host restore, abrupt-power-loss, or independent ACL-negative steps.
`integration/production-rehearsal` only records and validates operator-supplied
redacted observations; its self-test is synthetic.
