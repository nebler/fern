# Release and deployment tooling

See the [package map](../ARCHITECTURE.md#19-package-map).

This directory holds shell/Python release and deployment tools; it contains no
Go package. Host backup and restore are implemented in Go by `fern backup`
(`cmd/fern/backup.go`) and need no external runtime.

| Tool | Responsibility |
| --- | --- |
| `build-release.sh` | Validate SemVer and clean source state, build Linux amd64/arm64 binaries, collect release/image provenance fields, hash assets, and package distribution. |
| `create-release-bundle.py` | Create the deterministic release tar/gzip bundle from staged assets. |
| `verify-release-tag.sh` | Validate release-tag inputs for the publication workflow. |
| `test-release-workflow.sh` | Exercise release workflow contracts. |
| `test-deployment.sh` | Validate deployment assets and operator workflow assumptions. |
| `test-critical-coverage.sh` | Apply the repository's focused correctness/coverage checks. |

The release builder requires a clean tree and rechecks it before publishing its
local output. It records deterministic source time, Go version, checksums, and
explicit local-vs-published image metadata rather than fabricating attestations.
It stages systemd/configuration examples and the release, backup-manifest,
compatibility, and rehearsal schemas. Read each script's usage/environment
requirements before running it; a release build is not a harmless compile
command in an actively edited working tree.

## Current state and safety boundaries

The compatibility manifest declares task-store schema **9**, control-state schema
**2**, credential schema **1**, and no supported historical baseline. Runtime
resource spec **10** is a separate container identity contract. Release metadata
describes a pre-release reset, an offline age-encrypted `fern backup`, restore by
state-directory swap with a kept previous copy, and **no runtime volume exports**.

Backup and restore do not promise atomicity across filesystems, Docker, or
GitHub. The container harness owns run Git/PR work, and the removed host
publisher and verification services are not restored by these scripts.

Release cost is dominated by cross-compilation, checksumming, and compression;
it is unmeasured here.
