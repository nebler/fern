# Upgrade Harness

`./integration/upgrade/run.sh` validates Fern task-store compatibility from the
first repository-established baseline to the current schema.

`deploy/release/compatibility-manifest.json` identifies `baseline-v1` at task
schema 4. It is a checked-in fixture established by the repository, not a
historical release or tag. The harness verifies fixture metadata/checksums,
copies the exact baseline into isolation, opens it with current code, validates
the semantic schema-6 result, and checks that publication admission receipts and
legacy-row quarantine behave as required.

It then restores the verified pre-upgrade bytes as the rollback operation and
performs the upgrade again. Rollback therefore means restoring an offline backup
created before migration. It does not make schema 6 readable by older code, and
it does not claim atomicity across host filesystems, Docker volumes, or external
services.

Run from the repository root:

```bash
./integration/upgrade/run.sh
```
