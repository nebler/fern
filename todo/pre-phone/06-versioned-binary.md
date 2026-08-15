# Task 06: Add A Versioned Binary Build

## Commit

```text
build: produce versioned fern binaries with checksums
```

## Purpose

Install a known Fern binary on the target host without requiring a source checkout or local Go compilation.

## Dependencies

Start from task `00`. Independent of all other Wave 1 tasks.

## Owned Files

May modify `cmd/fern/main.go`.

May create:

```text
cmd/fern/version.go
cmd/fern/version_test.go
scripts/build-release.sh
```

Do not edit `Makefile`, CI, README, root example config, or Dockerfile. Task `09` integrates shared files.

## Contract

Add `fern version` reporting a semantic version or `dev` and an injected commit identifier.

The build script must:

- require an explicit version;
- build only host targets intended for the first deployment;
- inject version and commit with linker flags;
- use deterministic filenames in `dist/`;
- generate SHA-256 checksums;
- fail on partial output;
- avoid publishing or embedding credentials/local paths.

## Tests

- `run(["version"])` succeeds.
- Development builds report `dev`.
- Linker-injected values appear.
- Usage includes `version`.
- Missing version fails the script.
- Produced checksums verify.

## Acceptance

```bash
GOTOOLCHAIN=local go test ./cmd/fern
./scripts/build-release.sh v0.1.0-prephone
shasum -a 256 -c dist/SHA256SUMS
```

## Out Of Scope

GitHub Release publication, package repositories, signing/notarization, automatic version selection, and image publication.
