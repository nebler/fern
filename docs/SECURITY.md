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
- Pairing codes are capped, device names are bounded and escaped, replay is
  rejected, and durable device grants survive restart.
- Device cookies are `Secure`, `HttpOnly`, `SameSite=Strict`, and stripped before
  proxying to OpenCode. Upstream responses cannot set either the current or
  reserved future Fern device-cookie name; unrelated `Set-Cookie` headers are
  left untouched.
- Paired devices can use OpenCode but cannot access operator control or
  publication routes.
- Fern control credentials and common GitHub token variables are rejected from
  workspace configuration by key, by `${...}` source reference under an alias,
  and by post-expansion equality with the control credential.
- Host-credential publication is disabled unless a positive numeric GitHub
  repository ID and exact-case canonical `owner/repository` are configured
  together. The checkout origin cannot select authority. The durable journal
  fixes repository identity, base ref/SHA, result commit, branch, and complete
  final draft-PR proof before reporting success. Legacy effect-capable records
  are not automatically retried.
- Fern-owned HTML uses a restrictive CSP and control mutations perform an
  Origin check against the canonical operator origin, not client `Host`.
  Fetch Metadata rejects same-site siblings and cross-site browser mutations.
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

These controls are useful but do not complete the product boundary below.

The current publisher still obtains and transports the host user's potentially
account-wide `gh` token to bounded host `git` and `gh api` subprocesses. Child
environments and errors are sanitized, but this remains a prototype-only broad
credential boundary. It is not equivalent to a repository-scoped GitHub App
installation token.

## Release-Blocking Findings

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
CLI-only controls plus CSRF/Fetch Metadata hardening.

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
3. Rename the Fern device cookie into the `__Host-` namespace. Upstream
   `Set-Cookie` attempts targeting both the current and future reserved names are
   already removed while unrelated headers remain untouched.
4. Add bounded issuance rate policy in addition to the outstanding pairing-code
   cap when the product supports more than one trusted operator.

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
