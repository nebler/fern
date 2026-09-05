# Source verification and GitHub credential contract (runtime spec 10)

## Build-only source signature policy

Before `bun install` or any source build script runs, the build stage checks out
the exact pinned commit `39fb919a054190498f6d5b7985bde231f93ad7a6` and runs
`verify-source-signature`. It requires a successful `git verify-commit --raw`
and exactly one `VALIDSIG` whose full signing and primary fingerprints equal:

```text
968479A1AFF927E37D1A566BB5690EEEBB952194
```

This is the **GitHub web-flow** signing policy. It is not a maintainer signature
policy and is not independent verification of GitHub. The key was retrieved
from `https://github.com/web-flow.gpg`; `github-web-flow.asc` vendors only the
above primary key (armored GPG export), excluding the older web-flow key.
The verifier checks the complete primary fingerprint and rejects additional
primary keys before importing into a fresh private keyring. Automatic key
retrieval is disabled; no host keyring, ownertrust, short key ID, display name,
or network-fetched key is accepted as authorization during verification.
Missing, invalid, and unapproved signatures fail the build.

Rotation requires a reviewed change to both the vendored key and the literal
full fingerprint in the verifier. Confirm the replacement through GitHub's
published signing-key information and your organization's trusted review
process, inspect its primary fingerprint, and rerun the offline signature
fixtures plus a real image build. Do not automatically accept keys advertised
by an untrusted commit, or silently fall back to the previous key. Review key
revocations explicitly: an offline vendored key does not discover new ones.

This proves the pinned Git commit has a signature from the approved GitHub
web-flow key. It does **not** prove individual author/maintainer approval,
source safety, uncompromised GitHub infrastructure, dependency provenance,
reproducible builds, or runtime artifact signatures. GnuPG, the public key, and
the verifier are build-stage-only; the source commit/version/profile and runtime
spec remain unchanged because this is build-time integrity, not a runtime change.

Build and qualify a separate image without replacing harness/dev tags:

```sh
docker build --tag fern/opencode-background-source:signed images/opencode-background-source
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE=fern/opencode-background-source:signed \
FERN_OPENCODE_BACKGROUND_SOURCE_BUILD=0 \
python3 integration/opencode-background-source-contract/contract_harness.py
```

## GitHub credential contract

The harness supplies these files through the Docker API into the existing
`/home/user/.local/share/opencode` private data volume, never the workspace:

| File | Contents | Owner / mode |
| --- | --- | --- |
| `fern-github-token` | Current GitHub App installation token, optionally newline-terminated | `1001:1001`, `0600` |
| `fern-github-repository` | Exact `owner/repo`, optionally newline-terminated | `1001:1001`, `0600` |

Both readers reject symlinks, nonregular files, incorrect owner/mode, oversized
files, and malformed contents. The harness owns expiration and refresh (every
five minutes); use atomic replacement. Each invocation reads the current inode.
An already-running `gh` process retains the token supplied when it started.

- `/usr/local/bin/gh` (`0755`, root-owned) wraps the pinned GitHub CLI at
  `/usr/local/libexec/gh` (`0755`, root-owned). It supplies `GH_TOKEN` only to the
  child, removes alternate token variables and `GH_DEBUG`, disables prompts,
  and forces `GH_HOST=github.com`. All `gh auth` commands are disabled, including
  token-printing commands. `gh --version` works without credentials; other
  commands fail with a static, token-free error when credentials are unavailable.
- Every invocation uses a fresh `0700` `fern-gh-config-*` directory inside the
  data volume as `GH_CONFIG_DIR`, removed on normal completion. No personal gh
  configuration is mounted or reused. Forced termination may leave an empty
  temporary configuration directory; it is never reused.
- `/usr/local/bin/fern-git-credential` (`0755`, root-owned) handles only `get`.
  It emits `x-access-token` and the current token only for `protocol=https`,
  `host=github.com`, and the exact bound repository path (optionally `.git`).
  Foreign/malformed requests and missing credentials produce no output.
  `store` and `erase` do nothing. System Git config enables
  `credential.useHttpPath=true` and selects this fixed helper path.
- Shared readers live at `/usr/local/libexec/fern-github-credentials.cjs`
  (`0644`, root-owned). Node is already part of the final runtime image.

These wrappers avoid accidental credential persistence and cross-repository Git
credential use; they are not a sandbox against code running as UID 1001 (which
can read its token directly). GitHub App installation permissions remain the
authorization boundary. The harness owns pushes and PR creation.

Offline tests (no Docker, network, or real credentials; signature fixtures
require local `git`, `gpg`, and `gpgconf`, otherwise they are skipped). Signature
tests generate an ephemeral signing key and change only a temporary copy of the
verifier to test successful verification; the production policy has no
fingerprint override:

```sh
python3 -m unittest discover -s integration/opencode-background-source-contract -p 'test_*.py' -v
```

The image contract requires runtime spec label `10`, checks the wrapped pinned
`gh --version`, installed helpers, system Git configuration, and fail-closed
behavior without credential files. To test a separately built image without
overwriting the qualified tag:

```sh
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE=fern/opencode-background-source:harness \
FERN_OPENCODE_BACKGROUND_SOURCE_BUILD=0 \
python3 integration/opencode-background-source-contract/contract_harness.py
```
