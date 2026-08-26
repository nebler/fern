# GitHub Integration

This document defines Fern's GitHub authorization and publication boundary. A
limited, effect-narrowed host prototype remains in code and tests but is not
composed by `fern up`; its effecting routes reject use and only CLI dry-run
diagnostics remain. Separately,
`internal/githubapp` implements repository-scoped App credentials and exact REST
proofs, `internal/taskpublication` implements one-shot installation-token Git
transport, and `fern up` mounts the Manifest onboarding coordinator. Durable
task publication reconciliation is wired in App-broker mode, but no production
API prepares the initial publication journal record.

## Product Direction Decision

As of 2026-08-22, Fern's target GitHub workflow is Amp-style: the workspace has
an authenticated `gh` CLI, and the user explicitly chooses when to push or
create a draft pull request. Fern must not infer publication intent from
OpenCode becoming idle, a task appearing complete, or a clean commit existing.

This supersedes the earlier product decision that all GitHub credentials must
remain in a host-side GitHub App broker. The brokered App implementation remains
the accurate description of current code and a useful security/reconciliation
reference, but it is not the target user workflow. The two credential modes
must not be silently combined.

The authority contract is intentionally Amp-style: authenticated `gh` is
unrestricted inside the trusted workspace, and an explicit request in the user's
OpenCode prompt authorizes the agent to push or create a draft PR. Fern may add
audited phone actions as conveniences, but cannot claim they are an exclusive
publication gate while the same credential is available to OpenCode. Fern still
must implement phone-usable authentication, persistent credential state,
documented scopes, local logout and remote revocation, replacement, and backup
policy. Fern installs checksum-pinned `gh`, persists its isolated config,
executes the repository/base lookups needed by task admission through a bounded
attested container boundary, and supports durable task admission in
`workspace-gh` mode. Push and draft-PR creation remain ordinary workspace CLI
commands rather than a parallel Fern GitHub API.

## Retained Legacy Prototype (Unwired)

The following boundary describes the retained host-credential implementation,
not a currently reachable production mutation path. `fern up` does not supply a
legacy publication executor. Existing in-flight records need explicit operator
review; they are not resumed automatically.

Publication is disabled unless the strict workspace configuration binds both the
positive signed-64 GitHub repository ID and exact-case canonical full name:

```yaml
workspace:
  github:
    repository:
      id: 123456789
      fullName: owner/repository
```

Both values are required together. The owner is 1-39 ASCII alphanumeric or
non-edge hyphen characters. The repository is 1-100 ASCII alphanumeric,
period, underscore, or hyphen characters and cannot end in `.git`. The spelling
is retained exactly. Browser and ordinary CLI flags cannot override this
binding. Without it, `fern up` remains fully functional, does not resolve `gh`,
and omits the publication control.

`mode: github-app-broker` with `workspace.github.installationId` selects the
repository-scoped GitHub App lane. `mode: workspace-gh` forbids an installation
ID and uses only the managed workspace credential. The two credential modes are
never active in the same `fern up` process.

When this prototype was composed, the operator-authenticated control page on the
host-only listener owned its mutation path and required
`fern:$FERN_CONTROL_PASSWORD`;
OpenCode credentials and paired-device cookies have no publication authority,
Preparation first checks checkout `origin` against the configured full name as a
diagnostic, then queries `GET /repositories/{id}` through `gh api` and requires
exact ID, full name, owner, and name. Authority always comes from configuration,
never `origin`. It fetches the base, immediately resolves `FETCH_HEAD^{commit}`,
and uses that exact SHA for ancestry and diff policy.

Before push, the coordinator durably records repository ID/full name, exact base
ref/SHA, exact result commit, and Fern branch. Recovery uses only that complete
tuple, revalidates the configured binding and numeric repository route, and
refetches the configured base. Base movement is a conflict, never a rewrite.
Remote branch absence permits one non-force exact-SHA push; an exact remote SHA
is reusable, and any other SHA conflicts. A lost push response is resolved by
the exact remote ref read.

PR discovery uses REST with exact head and base. Zero candidates permit exactly
one creation attempt; one candidate is re-read by exact number; multiple or
conflicting candidates fail closed. A lost create response is reconciled by
discovery and is never blindly retried. Success atomically persists the positive
PR number, canonical URL, open/draft state, and complete target/base/head
repository, ref, and SHA observation. Every field must match the durable tuple.

`fern github publish --dry-run --title ...` retains preparation diagnostics.
Non-dry-run standalone publication is rejected before Docker, GitHub, or Git
effects because it cannot bypass durable coordinator persistence.

This prototype still uses the host user's broad `gh` credential. Its durable
journal narrows and reconciles the effect but does not provide the final
least-privilege product boundary below.

## Prototype Audit

The current boundary is useful but must not be described as repository-scoped
authorization:

| Concern | Current state | Required state |
| --- | --- | --- |
| Agent credential exposure | Legacy host transport withheld its token; current workspace-`gh` deliberately has a pinned binary and workspace credential | Keep the two authority modes explicit |
| Human authorization | Operator Basic auth at `/fern/control` | Attributable, explicit publication approval |
| GitHub credential | Existing host-user `gh` token, potentially account-wide | Short-lived installation token for one selected repository |
| Repository identity | Immutable configured numeric ID and full name, revalidated through REST; `origin` is diagnostic | GitHub App installation plus repository identity |
| Destination | Normal preparation chooses `fern/<workspace>/<operation>` | Recovery independently enforces the same namespace |
| Base | Base branch and exact fetched SHA are fixed, persisted, and revalidated before push | Bind the same tuple to task/result verification |
| Result provenance | Current clean repository `HEAD` | Successful verification record tied to task, attempt, and commit |
| Pull request proof | Exact-number REST re-read and complete proof are persisted | Preserve this proof with App credentials |

### Release-Blocking Gaps

1. A manually tracked OpenCode session is not evidence that the session
   produced, tested, or approved the repository's current `HEAD`.
2. The control page publishes immediately without showing the repository,
   immutable base, commit, changed paths, verification, or actor. Paired-device
   cookies intentionally have no publication authority.
3. Trusted executable ownership and the broad host credential remain weaker
   than the separately implemented repository-scoped App transport; this legacy
   control flow is not automatically upgraded to that transport.

### Immediate Hardening Gate

Before any real GitHub mutation through this prototype, finish the task/result
and verification binding, exercise restart reconciliation through the real
control surface in an isolated disposable repository, and explicitly accept the
broad host-token risk. The deterministic package fakes are the current safety
gate; the former standalone live mutation script is disabled.

## Current Brokered-App Design

The current task-publication implementation uses a private GitHub App per
appliance and keeps all GitHub credentials in the host process. OpenCode may
edit the working tree and create local commits, but it does not receive the App
private key, an installation token, a personal access token, an SSH key, or a
general GitHub API proxy.

The host exposes narrow capabilities:

```text
clone selected repository at base SHA
fetch selected repository
push exact commit to allocated Fern branch
create or update one draft pull request
read publication status
```

It does not expose tokens, arbitrary repository URLs, arbitrary refs,
force-push, merge, ref deletion, workflow updates, repository administration, or
raw GitHub API access.

## Current App Onboarding

The brokered App onboarding design is:

```bash
fern github connect
fern workspace create OWNER/REPOSITORY
```

`fern github connect` uses GitHub's App Manifest flow:

1. Fern creates a single-use state value and opens GitHub in the user's browser.
2. The user names a private App and grants the requested permissions.
3. GitHub redirects the browser to Fern through private Tailscale Serve.
4. Fern exchanges the temporary code outbound and stores the App ID and private
   key on the host.
5. The user installs the App and selects only the intended repository.
6. Fern discovers the installation and repository by immutable numeric IDs.

This is no-manual-token onboarding, not no-consent onboarding. GitHub and an
organization owner may still require explicit installation approval.

No public Fern service is required for the manifest callback, installation-token
minting, clone, fetch, push, pull-request creation, or polling. The callback is a
browser redirect, so an enrolled browser can reach the tailnet-only Fern host.

## Trust Boundary

```text
GitHub
   |
   | outbound HTTPS, one-hour installation token
   v
Fern host process
   - App private key
   - selected installation and repository IDs
   - branch policy and operation journal
   - task/publication coordinator
   |
   | host-side exact repository operations
   | no credential crosses into workspace configuration
   v
Workspace checkout <--- edited and committed by OpenCode container
```

No container socket is required for the first host-coordinated task journey. If
one is added later, any repository process can call it; socket possession must
therefore grant only the fixed workspace's narrow publication capability, never
arbitrary GitHub authority.

## Credentials

For each operation, Fern:

1. Signs a short-lived App JWT in the host process.
2. Requests an installation token restricted to one repository.
3. Downscopes permissions to the operation.
4. Uses the token only in host memory.
5. Reconciles the operation and discards or revokes the token.

Installation tokens expire after one hour. They must never be written to the
repository remote, command arguments, logs, Fern YAML, Docker environment,
OpenCode data, or operation journal.

Initial App permissions are:

| Permission | Access | Purpose |
| --- | --- | --- |
| Metadata | Read | Repository identity |
| Contents | Read/write | Clone, fetch, and allocated-branch push |
| Pull requests | Read/write | Draft PR creation and update |

Do not initially request Actions write, Checks write, Workflows write,
Administration, Deployments, Secrets, Environments, or organization permissions.
Repository CI remains authoritative.

## Host Broker

The broker belongs in the existing `fern up` process because that process owns
the workspace lease and repository path. Its first caller should be Fern's
host-side task and publication coordinator. Do not add a container socket merely
to reproduce the current control-page call path. If a concrete future workflow
requires an in-container client, use a narrow Unix socket mounted through a
read-only socket directory and treat possession as a fixed-workspace
capability.

Conceptual operations are:

```text
clone(repository_id, base_sha)
fetch()
push(operation_id, commit_oid)
open_pr(operation_id, title, body, base)
status(operation_id)
```

The repository and destination are inferred from workspace configuration, not
accepted from the caller. Every mutation requires an idempotent operation ID,
bounded input, a deadline, and a durable state transition.

Host-side Git must disable repository-controlled hooks and untrusted global
configuration. Otherwise a container-written pre-push hook could execute in the
host process boundary. The token should be supplied through a one-shot
credential channel rather than a URL or argument.

## Branch And Pull Request Policy

- One writable repository and branch per operation.
- Branch namespace: `fern/<workspace>/<operation-id>`.
- Base SHA fixed when work starts.
- Never push the default branch.
- Never create or delete tags, notes, pull refs, or arbitrary refs.
- Never force-push; updates must fast-forward from the recorded remote SHA.
- Reject workflow-file changes unless a later explicit permission and policy
  allows them.
- Create one draft PR and persist its number, head, base, and last published SHA.
- Do not approve, merge, enable auto-merge, or bypass repository rules.

After an ambiguous network result, Fern queries the remote branch or exact
head/base PR before retrying. An existing exact result is success; a different
remote SHA is a conflict.

## Sleep And Resume

Before releasing compute, durable work should be committed and optionally
published under explicit policy. Installation tokens are never part of a
workspace snapshot.

On wake Fern must revalidate the installation, selected repository, permissions,
remote branch, PR state, and expected head SHA, then mint a fresh token for the
next operation. Installation removal leaves local work intact but blocks new
GitHub operations.

## Webhooks And Offline Hosts

GitHub cannot deliver webhooks to private Tailscale Serve, and a sleeping or
offline host cannot acknowledge them. Webhooks are therefore outside the first
GitHub integration.

Reliable CI/review continuation later requires a public durable relay that:

- verifies GitHub signatures over the raw body;
- persists before returning success;
- deduplicates by delivery ID;
- queues by installation and repository;
- lets Fern pull through an authenticated outbound connection;
- treats events as hints and reconciles current GitHub state.

Do not enable Tailscale Funnel solely for webhooks. A shared hosted Fern App
would make its operator a GitHub authority capable of minting installation
tokens, so it must remain optional and explicit.

## Delivery Stages

1. **Current broad-credential prototype:** retain the exact-commit journal and
   reconciliation, but label it unsupported for normal repositories.
2. **Prototype hardening:** close the repository-retargeting, recovered-branch,
   immutable-base, repository-hardening, and post-create PR proof gaps above.
3. **Task/result contract:** make publication consume a successful immutable
   Result record rather than the repository's current `HEAD`.
4. **App broker prototype:** manually configure App ID, installation ID,
   repository ID, and private key; mint repository-scoped installation tokens
   for host clone/fetch, Fern-branch push, and draft PR creation.
5. **Self-hosted onboarding:** add Manifest flow, selected-repository install,
   permission verification, revocation handling, and host-side clone.
6. **Long-running loop:** add fresh credentials on wake, exact PR/CI status
   polling, bounded retries, and review continuation.
7. **Optional relay:** add durable public webhook ingress only when offline
   event delivery is required.

The following work can proceed in parallel, with explicit merge gates:

| Track | Can start | Must join before |
| --- | --- | --- |
| Prototype safety fixes and negative tests | Now | Any further live GitHub mutation |
| App JWT, installation-token, and secure-key-storage spike | Now, behind an internal interface | Repository onboarding or supported publication |
| Durable task/result/publication schema | After OpenCode exact-ID contract characterization | Publication is bound to verified work |
| Manifest and selected-repository onboarding | After the App broker proves manual configuration | First supported user onboarding |
| Mobile publication review UX | Against a fixed Result/Publication API contract | Paired-phone publication is enabled |
| CI/review polling and optional notifications | After exact PR identity is durable | Long-running follow-up claims |

The App lane and durable-task lane should run concurrently once their shared
publication contract fixes repository ID, base SHA, result commit, operation ID,
and actor. The container capability and webhook relay are not on the first
release critical path.

The first App foundation now exists in `internal/githubapp`: standard-library
RS256 JWT signing, immutable numeric installation/repository identity,
separate installation-wide discovery and repository-scoped publication token
requests, required permission and expiry
checks, bounded responses, redirect refusal, redacted errors, validated
PKCS#1/PKCS#8 RSA key parsing, bounded private-App Manifest generation, and
at-most-once Manifest code conversion. `CredentialStore` supplies atomic
host-only filesystem persistence with strict `0700`/`0600` and symlink checks;
this is permission protection, not encryption at rest. `OnboardingStateStore`
adds checksummed restart-safe, digest-only callback state with ten-minute expiry,
bounded outstanding flows, exact local return-path binding, and atomic one-use
consumption. `OnboardingHTTP` implements the Manifest coordinator
at exact routes `/fern/github/app/setup` and `/fern/github/app/callback`. Setup
accepts one local `return` path, persists state before returning an auto-submit
form, and callback claims before its one authorized exchange, saves credentials
before completion, and redirects only to that bound path. `fern up` mounts setup
behind Fern Basic authentication on the loopback operator listener. The callback
alone is reachable without a device cookie on the remote private HTTPS listener;
its random one-use state is its authorization, and ambient Authorization/Cookie
headers are removed before dispatch. Callback authority must match the canonical
remote origin exactly. If task policy is configured before App credentials
exist, an operator completes onboarding and restarts Fern to activate tasks.
The route is not mounted once valid credentials exist; replacement remains
disabled until backup and rotation have a complete recovery contract.
After successful conversion, the callback redirects only to the exact validated
loopback setup origin plus its persisted relative return path; it never exposes
the operator control route on remote ingress.

The state store persists only the state digest plus its non-secret flow binding.
A callback whose state was still pending before a Fern restart can therefore
recover its exact flow and complete normally without persisting the raw state.
A crash after callback claim still cannot determine whether Manifest conversion
or credential save took effect and never regains exchange authority. Every
`reconcile_only` result fails closed as recovery-required and is quarantined;
operators must inspect the host credential store and GitHub App settings before
beginning a new flow. Installation/repository selection UI and durable selection,
caching, revocation, encrypted backup, and key rotation remain. Configured
repository identity is revalidated against GitHub at task-service startup. The
Git transport itself now exists in
`internal/taskpublication`: it uses a single-use private askpass credential
file, exact commit refspecs, conditional force-with-lease, bounded full-output
digests, and read-only reconciliation for lost push or pull-request responses.

The package also now has a narrow repository/PR REST proof client. Every call
uses a fresh repository-scoped installation token, refuses redirects and
unbounded pagination, and validates numeric repository identity, canonical full
name, PR number/URL/state/draft, and complete base/head repository/ref/SHA
observations. Draft creation is attempted once; `internal/taskpublication`
resolves a lost response by discovery and exact-number re-read without retrying
the mutation.
Fork head identity is preserved in observations so the coordinator can reject
it rather than accidentally treating it as the configured target.

`internal/taskpublicationcoord` now binds that transport to migration-3 journal
phases. It commits push and PR-create authorization before each one-shot effect,
uses read-only reconciliation after restart, and completes only from the exact
remote branch and open-draft PR tuple. `fern up` reconciles publication journals
before starting task API servers and then runs one workspace-scoped loop. No
public API currently prepares a publication: adding one still requires a durable
idempotent user receipt and explicit publication authorization.

## Sources

- [Registering a GitHub App from a manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
- [Authenticating as a GitHub App installation](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)
- [Generating an installation access token](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app)
- [Creating a pull request](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request)
- [Validating webhook deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
