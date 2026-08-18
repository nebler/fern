# GitHub Integration

This document defines Fern's GitHub authorization and publication boundary. A
limited host-command prototype is implemented for the phone field demo; the
GitHub App and onboarding design remain proposed.

## Implemented Field Prototype

After the agent commits a clean worktree, the host operator can run:

```bash
fern github publish --title "Describe the change"
```

The command validates a standard GitHub checkout, rejects dangerous local Git
configuration, submodules, workflow changes, dirty state, unsupported refs, and
non-GitHub origins. It obtains the existing host `gh` credential in memory,
fetches the current base, requires `HEAD` to descend from it, pushes exactly
`HEAD` without force to `fern/<workspace>/<operation>`, and creates or reuses a
draft PR. Publication acquires the workspace lease and requires the container
to be absent, so stop `fern up` and run `fern down` first. Run publication as
the same host user that runs Fern. `FERN_GITHUB_TOKEN`, `GH_TOKEN`, and
`GITHUB_TOKEN` are rejected from workspace environment so the obvious
credential path into Docker is closed.

The operator-authenticated Fern control page can perform the same operation
without SSH. It requires `fern:$FERN_CONTROL_PASSWORD`; OpenCode credentials and
paired-device cookies have no publication authority.
It records a workflow-associated publication request, stops idle compute while
holding request admission and lifecycle wake serialization closed, records the
exact repository/base/commit/branch before push, then records the draft PR URL
or a retryable failure. Daemon startup resumes `requested` and `pushing`
operations once. Prepared recovery queries the recorded remote branch first,
accepts only the exact SHA, rejects conflicts without force, and reconciles an
exact draft PR after a lost creation response. PR lookup failure never falls
through to creation. The browser cannot supply repository paths, remotes,
refspecs, or credentials.

This prototype still uses the host user's broad `gh` credential. Its durable
journal narrows and reconciles the effect but does not provide the final
least-privilege product boundary below.

## Decision

Fern should use a private GitHub App per appliance and keep all GitHub
credentials in the host process. OpenCode may edit the working tree and create
local commits, but it must not receive the App private key, an installation
token, a personal access token, an SSH key, or a general GitHub API proxy.

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

## User Experience

The intended onboarding is:

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
   |
   | narrow Unix-socket operations, no credentials
   v
OpenCode container
   - working tree and local Git objects
   - request to publish one exact commit
```

Any repository process can call a socket mounted into the container. Socket
possession must therefore grant only the fixed workspace's narrow publication
capability, never arbitrary GitHub authority.

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
the workspace lease and repository path. A small client inside the image may
request operations over a Unix socket mounted through a read-only socket
directory.

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

1. **Disposable field test:** use a short-lived, single-repository credential
   only through an explicit reviewed procedure, then revoke it.
2. **Broker prototype:** manually configure App ID, installation ID, repository
   ID, and private key; implement host clone/fetch, Fern-branch push, and draft
   PR creation.
3. **Container capability:** add the narrow Unix socket and client, operation
   journal, branch enforcement, redaction, and failure reconciliation tests.
4. **Self-hosted onboarding:** add Manifest flow, selected-repository install,
   permission verification, and host-side clone.
5. **Long-running loop:** add fresh leases on wake, PR status polling, CI/review
   continuation, bounded retries, and revocation handling.
6. **Optional relay:** add durable public webhook ingress only when offline event
   delivery is required.

## Sources

- [Registering a GitHub App from a manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
- [Authenticating as a GitHub App installation](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)
- [Generating an installation access token](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app)
- [Creating a pull request](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request)
- [Validating webhook deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
