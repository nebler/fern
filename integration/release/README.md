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
The release manifest also marks signatures as not generated; SHA-256 checksums
detect corruption but do not establish artifact authenticity.
