# Release harness

`./integration/release/run.sh` exercises the release builder in isolated, clean
Git fixtures at different paths. It builds twice from a fixed commit, compares
every output, verifies manifest hashes and `SHA256SUMS`, proves a modified binary
is rejected, and checks dirty-tree and malformed-version failures.

The harness also makes static assertions over the packaged systemd unit and the
documented Tailscale Serve command. It never invokes systemd, Tailscale, Docker,
or GitHub. Its evidence bundle is written beneath
`integration/release/artifacts/` by default, or to the empty directory selected
by `FERN_RELEASE_ARTIFACTS`.

The harness also runs `scripts/fern-host-backup.py` against deterministic local
fixtures. It proves exclusive epoch locking, credential and named-volume
segregation, byte-reproducible payloads, checksum/symlink/path-escape/hardlink
rejection, destructive fresh restore, generated transaction receipts,
previous-generation rollback, and source plus target epoch fencing.
Docker volumes are represented by explicitly pre-exported fixture directories;
the harness never invokes Docker or claims physical-host crash atomicity.
When run locally, the release manifest marks signatures/provenance as not
generated; SHA-256 checksums detect corruption but do not establish artifact
authenticity. The tag workflow separately requires a signed annotated tag,
publishes a digest-bound multi-architecture image, generates an SPDX SBOM,
creates GitHub provenance attestations, keylessly signs and attests the image,
verifies those claims, and attests every release asset before creating a GitHub
Release. The local harness does not emulate or claim those GitHub/Sigstore
effects. See [`docs/RELEASE_POLICY.md`](../../docs/RELEASE_POLICY.md).
