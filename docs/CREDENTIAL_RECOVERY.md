# Credential Recovery

Fern provides encrypted offline custody and rollback-safe local replacement for
the active GitHub credential in either `github-app-broker` or `workspace-gh`
mode. It does not encrypt active credentials at rest and cannot revoke a key or
OAuth token at GitHub.

## Preconditions

Every credential command:

- loads the exact deployment configuration and protected env file;
- acquires the offline workspace lease, so `fern up` must be stopped;
- requires workspace compute to be absent, so run `fern down` first;
- binds the bundle to workspace name, mode, hostname, installation ID when
  applicable, numeric repository ID, and exact repository full name.

App bundles additionally bind the App ID. Rotation cannot change App identity.
Workspace-`gh` bundles contain the managed volume export and are validated for
one exact host credential.

## Export

Create at least one age X25519 identity using an approved age implementation and
keep the private identity outside the Fern host backup. Export to every required
recipient:

```bash
fern credentials export \
  --config /etc/fern/fern.yaml \
  --env-file /etc/fern/fern.env \
  --recipient age1PRIMARY \
  --recipient age1RECOVERY \
  --output /secure/fern-github-credentials.age
```

Fern snapshots the current App credential file or workspace-`gh` volume, writes
an age-encrypted bundle with mode `0600`, and prints its generation plus SHA-256
fingerprint. Record the fingerprint separately. Do not store the identity beside
the encrypted bundle or in the general Fern backup.

## Import

Import performs rollback-safe replacement with an already prepared bundle. It
requires an active current credential because Fern must snapshot and encrypt the
prior generation before replacement; it is not a bootstrap path for an empty
host:

```bash
fern credentials import \
  --config /etc/fern/fern.yaml \
  --env-file /etc/fern/fern.env \
  --identity /secure/fern-age-identity.txt \
  --input /secure/fern-github-credentials.age \
  --rollback-output /secure/prior-generation.age
```

Before replacement Fern:

1. Decrypts the candidate in memory and validates its closed schema.
2. Requires the complete binding to equal current configuration.
3. Performs live GitHub identity, repository, and permission validation.
4. Rechecks that compute is absent.
5. Snapshots the active credential and writes an encrypted rollback artifact.
6. Rechecks absence and activates the candidate.

By default the rollback recipient is derived from the supplied X25519 identity.
Use repeatable `--rollback-recipient` flags when the identity format does not
provide one or custody policy requires different recipients.

An App save failure automatically attempts to restore the prior App credential.
A workspace-`gh` replacement failure retains the encrypted rollback artifact
for explicit import. Do not delete that artifact until the new credential has
passed operational validation.

## Rotate

Rotation uses the same validation and rollback sequence but requires explicit
acknowledgment of the external revocation obligation:

```bash
fern credentials rotate \
  --config /etc/fern/fern.yaml \
  --env-file /etc/fern/fern.env \
  --identity /secure/fern-age-identity.txt \
  --input /secure/next-generation.age \
  --rollback-output /secure/superseded-generation.age \
  --acknowledge-external-revocation
```

After local activation:

1. Start Fern and validate readiness plus one bounded GitHub identity/read path.
2. In App mode, verify the intended App installation and repository remain
   selected. In workspace-`gh` mode, verify `gh auth status` in the workspace.
3. Revoke the superseded App private key or OAuth token in GitHub.
4. Verify the superseded credential no longer works using an approved external
   procedure that does not expose it in logs or argv.
5. Retain the encrypted rollback only as long as policy permits. Revocation may
   intentionally make that artifact unusable and is not reversible by Fern.

## Limits

- Active App credentials are permission-protected plaintext files.
- Active workspace-`gh` credentials are plaintext inside the managed volume and
  intentionally available to trusted workspace code.
- Fern does not manage age private identities.
- Fern cannot revoke or prove revocation of GitHub credentials.
- Import/rotation cannot initialize an empty credential store or missing
  workspace-`gh` volume. Bootstrap through App onboarding or `gh auth login`, or
  restore the complete verified host backup, before using replacement.
- Live validation depends on GitHub availability and organization policy.
- Full host backup's separate credential tar is mode `0600` but not age-
  encrypted; it still requires encrypted external custody.
