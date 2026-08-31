# Release Policy

This document describes the checked-in release mechanism. It does not claim
that any release or tag has been created.

## Local Build

`scripts/build-release.sh <version>` requires a clean Git tree and emits
deterministic Linux amd64/arm64 binaries, deployment assets, release manifest
schema 2, and SHA-256 inventories. The builder embeds version and commit, uses
`CGO_ENABLED=0`, `-trimpath`, and no Go build ID.

`integration/release/run.sh` builds twice in clean fixtures at different paths,
compares every output, verifies manifests/checksums, tests corruption and dirty-
tree rejection, and exercises the low-level backup archive utility. Local
checksums detect corruption but provide neither signer identity nor provenance.

## Tag Admission

The GitHub release workflow runs only for `v*` refs and fails unless:

1. The name is semantic-version shaped.
2. The ref is an annotated tag, not a lightweight tag.
3. The tag directly targets the workflow commit.
4. GitHub reports the annotated tag signature as cryptographically verified and
   valid.
5. Local and GitHub tag object IDs match.

Both validation and publication jobs recheck this identity. A tag existing in
the repository is not itself proof that the workflow completed.

## Validation Gates

Before publication the workflow runs:

- repository formatting, race tests, vet, critical coverage, and CGO-free task
  store tests;
- deployment static checks;
- reproducible release harness;
- schema compatibility/upgrade harness;
- production rehearsal recorder self-test with synthetic facts;
- release-workflow policy checks;
- production image build, UID/GID and exact OpenCode smoke tests;
- real Docker lifecycle integration;
- authoritative source Background Run qualification against one exact local
  image ID, using only a zero-cost fake provider.

The rehearsal self-test validates the recorder, not physical infrastructure.
Physical reboot, replacement-host restore, real TLS/WSS phone behavior, and
independent ACL denial remain external acceptance gates.

The source Background Run candidate is built and qualified only inside
validation. The checked-in release workflow does not publish or promote that
image, and its local image ID is not a portable registry digest.

## Published Image

After validation, the workflow:

1. Builds and pushes Linux amd64/arm64 image manifests to GHCR.
2. Records the immutable multi-architecture digest.
3. Generates and validates an SPDX JSON SBOM.
4. Creates GitHub build-provenance attestation for the OCI subject.
5. Keylessly signs the digest with Cosign from the release workflow identity.
6. Keylessly attests the SPDX SBOM to that digest.
7. Verifies the Cosign signature/SBOM attestation and GitHub provenance before
   building release assets.

The release bundle records the digest, image reference, certificate identity,
OIDC issuer, SBOM, and provenance URL. Deploy by digest, not a mutable tag.

## Release Assets

The workflow builds the final bundle only after image verification, verifies
`SHA256SUMS`, and creates GitHub build-provenance attestations for every file in
`dist`. It verifies each asset attestation against repository, workflow, source
ref, and source commit before creating the GitHub Release.

Binary and deployment assets have GitHub provenance attestations but no separate
Cosign/GPG binary signature. The signed annotated source tag and attested assets
are different controls; operators must verify both.

## Operator Verification

For an actual release, verify at minimum:

```bash
gh attestation verify PATH_TO_ASSET \
  --repo OWNER/REPOSITORY \
  --signer-workflow OWNER/REPOSITORY/.github/workflows/release.yml \
  --deny-self-hosted-runners
shasum -a 256 -c SHA256SUMS
cosign verify \
  --certificate-identity 'https://github.com/OWNER/REPOSITORY/.github/workflows/release.yml@refs/tags/VERSION' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/OWNER/fern/opencode@sha256:DIGEST
```

Also verify the release manifest's version, commit, image digest, signature
status, provenance status, and SBOM digest. Use the exact commands and identities
published with that release rather than copying placeholders literally.

## Compatibility And Rollback

The compatibility manifest names `baseline-v1`, task-store schema 4, as the
first repository-established baseline. It is explicitly not a historical
release or tag. Current schema is 6.

Upgrade testing migrates the fixture, validates semantics, restores exact
pre-upgrade bytes, and upgrades again. Production rollback is the same model:
take and verify an offline pre-upgrade backup, and restore those bytes if needed.
Older code must not open the migrated database. The workflow does not make
cross-filesystem, Docker-volume, or external-service changes atomic.

## External Release Gates

The workflow does not establish:

- a physical Ubuntu/systemd install and reboot;
- replacement-host restore or abrupt-power-loss behavior;
- real private-edge TLS/WSS and physical phone revocation;
- independent tailnet ACL denial;
- provider-funded terminal execution, billing, or interruption behavior;
- organization-specific GitHub policy acceptance;
- external revocation of superseded credentials.

Record these separately with `integration/production-rehearsal` where applicable.
Never treat its synthetic self-test as evidence that the physical steps occurred.
