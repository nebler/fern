# Physical production rehearsal

This harness records redacted operator evidence for a real source-host reboot,
backup, fence, replacement-host restore, and external access rehearsal. It does
not reboot hosts, run backups, restore data, mutate Tailscale, open browsers, or
claim those operations happened. Operators perform and independently verify
each physical or external step, prepare the phase JSON described by the schema,
and explicitly confirm the observation when recording it.

The durable state is `evidence.json` in an operator-selected directory. Writes
are locked, validated, and atomically replaced, so the same directory can be
resumed after a reboot or transfer to the replacement host. The directory is
created mode `0700` and the state file mode `0600`.

## Start and resume

Use pseudonymous IDs, not names, emails, serial numbers, IP addresses, or other
identifiers. Choose a directory outside the repository when recording real
evidence.

```sh
./integration/production-rehearsal/run.sh init \
  --evidence /secure/operator/fern-rehearsal-20260827 \
  --rehearsal-id rehearsal-20260827-a \
  --operator-id operator-a

./integration/production-rehearsal/run.sh status \
  --evidence /secure/operator/fern-rehearsal-20260827
```

For each phase, create a JSON object containing exactly that phase's `evidence`
fields from
`deploy/release/production-rehearsal-evidence.schema.json`. Record only derived
facts, booleans, pseudonymous IDs, timestamps, and SHA-256 digests. Do not store
command output, logs, headers, configuration, URLs with credentials, key
material, tokens, cookies, backup paths, or backup contents.

```sh
./integration/production-rehearsal/run.sh record \
  --evidence /secure/operator/fern-rehearsal-20260827 \
  --phase source-preflight \
  --input /secure/operator/source-preflight.json \
  --confirm 'CONFIRM source-preflight rehearsal-20260827-a'
```

The confirmation must exactly identify the next phase and rehearsal. A phase is
not a request for the harness to perform work; it is the operator's assertion
that the supplied physical or external observation was completed and redacted.
Run `status` after moving the bundle to discover the next required phase.

## Required phases

1. `source-preflight`: Observe source host and boot IDs, release version and commit, immutable image digest, active service, and exact HTTPS tailnet origin.
2. `pre-reboot`: Re-observe the source host, boot ID, and active service, then explicitly request the physical reboot outside this harness.
3. `post-reboot`: Prove the boot ID changed while host identity remained stable and the service recovered.
4. `source-backup`: Run the real backup procedure outside this harness and record its pseudonymous ID, manifest and payload digests, credential policy, and successful verification.
5. `source-fence`: Disable the source service and origin outside this harness; record the fence ID and inactive facts before restoring the target.
6. `target-restore`: Restore outside this harness and record the distinct target host and boot IDs, matching release/image/backup facts, transaction digest, and active service.
7. `tls-wss`: From an external observer, verify certificate hostname validation, TLS, and WSS against the exact origin.
8. `phone`: From a physical iOS or Android phone on cellular or external Wi-Fi, verify page load, WSS, and an operator action.
9. `acl-negative`: Revoke that phone outside this harness, observe its later ACL denial, and record an independent observer plus a different authorized control device that still succeeds.
10. `finalize`: Recheck the source fence and target service, review the complete JSON for redaction, and explicitly confirm completion.

Cross-phase validation requires stable source identity, changed reboot identity,
matching release/image/backup/origin facts, a target distinct from the source,
the positively tested phone as the revoked device, chronological revocation and
denial, and an ACL observer distinct from the recording operator and revoked
device.

## Validation

Validate partial state at any time. `--require-final` rejects all bundles that
have not completed every phase in order, including `finalize`.

```sh
./integration/production-rehearsal/run.sh validate \
  --evidence /secure/operator/fern-rehearsal-20260827

./integration/production-rehearsal/run.sh validate --require-final \
  --evidence /secure/operator/fern-rehearsal-20260827
```

The recorder rejects unknown/missing fields, unsuccessful required booleans,
malformed facts, multiline values, common secret-bearing field names and token
patterns, URL credentials, out-of-order phases, inconsistent cross-phase facts,
and finalization with missing phases. This is defense in depth, not a secret
scanner guarantee; the operator's final redaction review remains required.

## Local self-test

```sh
./integration/production-rehearsal/run.sh self-test
python3 -m json.tool deploy/release/production-rehearsal-evidence.schema.json >/dev/null
```

Self-test uses temporary synthetic facts only. It tests phase ordering, durable
resume, malformed evidence rejection, secret-like field rejection, the missing
phase finalization gate, schema phase parity, and a complete valid rehearsal. It
does not exercise or represent physical production infrastructure.

No checked-in evidence bundle is implied by this harness. A release workflow
running `self-test` proves only recorder behavior, never completion of the ten
operator phases.
