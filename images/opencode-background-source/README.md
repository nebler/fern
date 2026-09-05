# GitHub credential contract (runtime spec 10)

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

Offline tests (no Docker, network, or real credentials):

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
