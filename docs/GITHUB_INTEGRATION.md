# GitHub Integration

Fern supports two explicit, mutually exclusive GitHub authority modes. Neither
mode infers publication from idle compute, a clean checkout, or an apparently
finished OpenCode session.

| Mode | Credential boundary | Mutation model |
| --- | --- | --- |
| `workspace-gh` | Persistent `gh` credential available to trusted workspace code | User or agent runs ordinary `git` and `gh`; effects are outside Fern's receipt journal |
| `github-app-broker` | App private key remains on the host; short-lived token is scoped to one repository | Paired client admits one exact verified result; Fern journals and reconciles branch and draft-PR effects |

GitHub access is disabled when `workspace.github` is absent. Configuration binds
the positive numeric repository ID and exact canonical full name. Checkout
`origin` is never repository authority.

## Workspace `gh`

The image checksum-pins GitHub CLI and Fern mounts a dedicated persistent config
volume at `/home/user/.config/gh`. It never imports the host user's `gh`
credential. Authenticate and inspect it inside the OpenCode terminal:

```bash
gh auth login --hostname github.com
gh auth status --hostname github.com
gh auth setup-git --hostname github.com
```

An explicit prompt may authorize the trusted agent to run `git push` or `gh pr
create --draft`. Fern cannot claim that a phone button is the exclusive
publication gate while the same credential is available to workspace code.
Direct mutations have no Fern idempotency receipt or lost-response
reconciliation, and their scope is whatever GitHub granted to that credential.

Fern does use a bounded, image-attested `gh api` execution to resolve a task's
base ref. It validates the configured numeric repository and exact full name.
For a newly created workspace binding this first live check occurs when a task
resolves its base; correcting a mistaken immutable binding requires operator
state recovery.

Remove local access with `gh auth logout --hostname github.com` and revoke the
credential in GitHub. Fern cannot perform that external revocation.

## GitHub App Broker

The App is private and initially requests only:

| Permission | Access |
| --- | --- |
| Metadata | Read |
| Contents | Read/write |
| Pull requests | Read/write |

It requests no webhook events. Fern does not grant Actions, Workflows,
Administration, Deployments, Secrets, Environments, or organization authority.

### Onboarding

When App credentials are absent, the operator starts the Manifest flow from the
loopback-only control page. Setup requires Fern Basic authentication. The exact
callback alone is reachable through the configured private HTTPS origin and is
authorized by a random, one-use, ten-minute state value.

Fern stores only the state digest and non-secret local return-path binding.
Pending state survives restart. Callback claim commits before Manifest code
conversion; a crash after claim never restores exchange authority. Ambiguous
conversion is quarantined for operator inspection. Ambient `Authorization` and
`Cookie` headers are removed before callback dispatch, and success redirects
only to the validated loopback setup origin plus the stored relative path.

Valid credentials disable setup. Installation/repository discovery primitives
exist, but selection is still configured by the operator and first onboarding
requires a Fern restart to activate task services.

### Active Credentials

App credentials live below `$HOME/.fern/github-app` in a private `0700`
directory and `0600` regular file with ownership, type, symlink, and atomic-save
checks. Active credentials are protected by filesystem permissions, not
encrypted at rest.

Fern signs short-lived RS256 App JWTs. Installation-wide discovery tokens and
repository-scoped operation tokens are separate. Each REST or publication
operation obtains a fresh token, validates repository identity, required
permissions, and expiry, and refuses redirects or unbounded responses.

Use `fern credentials export|import|rotate` for age-encrypted offline custody,
first-generation import, and rollback-safe local replacement. Rotation cannot
bootstrap or revoke the superseded App key at GitHub;
`--acknowledge-external-revocation` makes that obligation explicit. See
[Credential Recovery](./CREDENTIAL_RECOVERY.md).

## Publication Admission

App publication begins only at:

```http
POST /fern/api/v1/results/{resultId}/publications
Idempotency-Key: publish-command-1
Content-Type: application/json

{"expectedVerificationId":"ver_..."}
```

The client can provide only the result ID, successful verification ID, and
idempotency key. One SQLite transaction validates:

1. Active App authority for the configured workspace and repository.
2. The exact current task/attempt ownership and cancellation fence.
3. A sealed `changed` result for that task.
4. A successful verification of the same result commit.
5. No existing publication for the result.
6. The actor-owned idempotency claim and canonical request hash.

Fern then derives the installation, repository, base ref/SHA, result commit,
operation ID, deterministic branch, expected old SHA, and broker policy. The
same transaction inserts the receipt, actor snapshot, journal event, and
prepared publication. An exact replay returns the original `202` projection and
sets `Idempotency-Replayed: true`; changed input or actor conflicts without
disclosing the existing target.

Task-store schema 6 requires `publications.admission_receipt_id`. Publications
migrated from an older schema have a null receipt: worker discovery excludes
them and SQL triggers reject updates. Migration never invents historical user
authority.

## Effect Journal

Publication consumes only the immutable sealed and verified commit. It never
publishes mutable current `HEAD`.

```text
none -> push_started -> push_observed -> pr_create_started -> published
```

The coordinator discovers work, acquires `AcquirePaused`, re-reads the identical
publication/task/attempt/result/verification tuple, and retains the fence
through each Git/GitHub call and store transition. This prevents workspace code
from changing Git configuration around a credential-bearing push.

Rules:

1. Commit `push_started` before one exact commit push.
2. Supply the token through a private one-use askpass file, never argv, logs,
   evidence, repository config, Docker environment, or SQLite.
3. Use an exact refspec and `--force-with-lease` against the recorded old SHA;
   never blind-force, delete, or retarget a ref.
4. After a lost push response or restart at `push_started`, read the exact branch
   only. Never push again.
5. Commit `push_observed` only when the branch equals the result commit.
6. Read-reconcile an exact open draft PR before creation.
7. Commit `pr_create_started` before one create call. After ambiguity, discover
   and re-read; never create again.
8. Complete only when repository, PR number/URL, open/draft state, base
   repository/ref/SHA, and head repository/ref/SHA all match the durable tuple.

Temporary read ambiguity becomes `uncertain`. Contradiction becomes `conflict`
or `recovery_required`. No timeout or absent observation grants mutation retry.
Task snapshots expose bounded publication state and exact draft-PR summary.

## Retired Host Publisher

The old host-user-`gh` package remains for tests and dry-run diagnostics but is
not composed by `fern up`. Effecting legacy routes return `410 Gone`; standalone
non-dry-run publication is rejected. `fern github publish --dry-run` does not
mutate GitHub.

Unresolved retired JSON publication records block readiness as the fixed
`legacy-publication` component. Stop `fern up`, inspect the records, then run:

```bash
fern debug quarantine-publications --config /etc/fern/fern.yaml
```

The command holds the workspace lease and atomically marks unresolved records
quarantined. It does not replay, complete, or delete their effects.

## Webhooks And Follow-Up

Fern requests no webhooks because GitHub cannot deliver directly to a private,
possibly sleeping Tailscale Serve host. CI/PR polling, notifications, and review
continuation are not implemented. Do not enable Funnel solely for webhooks.

## Remaining Limits

- Active App and workspace-`gh` credentials are not encrypted at rest.
- External key/token revocation is manual.
- Encrypted import can bootstrap an empty credential store or missing `gh`
  volume, but that first activation has no prior rollback generation. Rotation
  requires a prior generation.
- App installation/repository selection has no complete onboarding UI.
- First credential activation after onboarding requires restart.
- The embedded phone task page displays publication state but does not yet offer
  the App publication command; API clients must retain their idempotency key.
- Workspace-`gh` intentionally gives trusted workspace code the credential's
  full granted scope and no Fern effect receipts.
- No checked-in test claims a live organization-specific GitHub policy or
  provider-funded publication rehearsal occurred.

## Sources

- [Registering a GitHub App from a manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
- [Authenticating as a GitHub App installation](https://docs.github.com/en/apps/creating-github-apps/authenticating-as-a-github-app-installation)
- [Generating an installation access token](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token)
- [Creating a pull request](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request)
