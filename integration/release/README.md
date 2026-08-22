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

The transaction manifest is a shape for future upgrade and rollback work, not
an implementation. Its evidence explicitly marks state-schema compatibility,
SQLite backup, application-secret backup, and executable rollback as not
implemented. No backup capability should be inferred from these placeholders.
The release manifest also marks signatures as not generated; SHA-256 checksums
detect corruption but do not establish artifact authenticity.
