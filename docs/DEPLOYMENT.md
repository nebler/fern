# Supervised Private Deployment

This runbook covers one trusted owner and workspace on Ubuntu Server 24.04 with
systemd, local Docker Engine, and private Tailscale Serve. Fern and OpenCode are
not hostile multi-user isolation. Docker-group membership is effectively root.

Tailscale terminates HTTPS and forwards only Fern's remote listener. Never
enable Funnel and never publish the operator listener.

Automated gates cover static deployment policy, browser behavior, simulated
orderly shutdown, schema upgrade/rollback fixtures, reproducible builds, and
isolated backup/restore. The production rehearsal recorder self-tests with
synthetic facts. No checked-in evidence proves a physical target-host install,
reboot, replacement-host restore, real private TLS/WSS phone path, or independent
ACL-negative exercise.

## Trust And Authentication

Fern gives OpenCode read/write access to the selected repository. OpenCode and
repository code are inside the trusted workspace boundary and may access
provider credentials intentionally forwarded there.

The operator listener accepts two distinct Basic credentials:

- `opencode:$OPENCODE_PASSWORD` for the official local OpenCode CLI.
- `fern:$FERN_CONTROL_PASSWORD` for host-only Fern administration.

The values must differ and the Fern control password must contain at least 32
characters. Both credentials are rejected by remote ingress before wake. Paired
phones receive a digest-backed, expiring, revocable device cookie and cannot
access operator controls.

GitHub is optional. `workspace-gh` stores `gh` state in a dedicated Docker
volume available to trusted workspace code. `github-app-broker` keeps the App
private key on the host and uses short-lived repository-scoped tokens. The modes
are mutually exclusive. See [GitHub Integration](./GITHUB_INTEGRATION.md).

The pinned OpenCode profile cannot prove generic terminal success. A user seal
authorizes one exact clean repository snapshot under `AcquirePaused`; it marks
the attempt `superseded`, not `succeeded`. Optional host-owned verification runs
after sealing. App publication requires that exact sealed changed result and a
successful verification of the same commit.

## 1. Install Dependencies

Install Git, Docker Engine, and Tailscale from their supported Ubuntu
repositories. Building from source also requires the Go version in `go.mod`.

- <https://docs.docker.com/engine/install/ubuntu/>
- <https://tailscale.com/kb/1031/install-linux>

```bash
sudo systemctl enable --now docker tailscaled
sudo tailscale up
sudo docker version
sudo tailscale status
```

Review tailnet grants so only intended principals can reach this host. Fern
needs no public or LAN firewall listener.

## 2. Create Account And Paths

| Purpose | Path |
| --- | --- |
| Reviewed Fern source | `/opt/fern/src` |
| Installed binary | `/usr/local/bin/fern` |
| Configuration | `/etc/fern/fern.yaml` |
| Protected environment | `/etc/fern/fern.env` |
| Service home and Fern state | `/var/lib/fern` |
| Workspace repository | `/srv/fern/workspace` |
| Backups | `/var/backups/fern` |

The default image and service account use UID/GID 1001.

```bash
getent passwd 1001 && { echo 'UID 1001 already allocated' >&2; exit 1; }
getent group 1001 && { echo 'GID 1001 already allocated' >&2; exit 1; }
sudo groupadd --gid 1001 fern
sudo useradd --uid 1001 --gid 1001 --home-dir /var/lib/fern \
  --create-home --shell /usr/sbin/nologin fern
sudo usermod -aG docker fern
sudo install -d -o root -g root -m 0755 /opt/fern
sudo install -d -o root -g fern -m 0750 /etc/fern
sudo install -d -o fern -g fern -m 0750 /var/lib/fern /srv/fern
sudo install -d -o fern -g fern -m 0700 /var/backups/fern
sudo -u fern git clone https://EXAMPLE.invalid/OWNER/REPOSITORY.git \
  /srv/fern/workspace
```

Keep Fern source and the repository controlled by OpenCode separate.

## 3. Select Artifacts

Deploy a reviewed full commit, never a moving branch. A local source build emits
reproducible Linux amd64/arm64 binaries and checksums from a clean tree, but local
output is not signed or provenance-attested.

```bash
export FERN_COMMIT=REPLACE_WITH_FULL_COMMIT_SHA
export FERN_VERSION=v0.1.0
sudo git clone https://github.com/nebler/fern.git /opt/fern/src
sudo git -C /opt/fern/src checkout --detach "$FERN_COMMIT"
test "$(sudo git -C /opt/fern/src rev-parse HEAD)" = "$FERN_COMMIT"
cd /opt/fern/src
sudo env GOTOOLCHAIN=local ./scripts/build-release.sh "$FERN_VERSION"
ARCH=$(dpkg --print-architecture)
case "$ARCH" in amd64|arm64) ;; *) exit 1;; esac
sudo install -o root -g root -m 0755 \
  "dist/fern-${FERN_VERSION}-linux-${ARCH}" /usr/local/bin/fern
sudo docker build --pull -t "fern/opencode:$FERN_COMMIT" images/opencode
sha256sum /usr/local/bin/fern
/usr/local/bin/fern version
```

For a tagged release, follow [Release Policy](./RELEASE_POLICY.md) and verify the
published asset provenance, digest-bound image signature/provenance/SBOM
attestation, release manifest, and checksums. No release or tag is claimed by
this document.

## 4. Install Configuration

```bash
sudo install -o root -g fern -m 0640 \
  deploy/systemd/fern.yaml.example /etc/fern/fern.yaml
sudo install -o root -g fern -m 0640 \
  deploy/systemd/fern.env.example /etc/fern/fern.env
sudoedit /etc/fern/fern.yaml
sudoedit /etc/fern/fern.env
```

The env file contains distinct random `OPENCODE_PASSWORD` and
`FERN_CONTROL_PASSWORD` values and only required provider keys. Do not place
secrets in YAML, unit arguments, or Git. Root and Docker administrators can
inspect credentials forwarded into the container.

Set `proxy.remoteOrigin` to this host's exact lowercase Tailscale HTTPS root,
without a path, trailing slash, or explicit `:443`. The parseable example value
must be replaced before service start.

```yaml
workspace:
  name: demo
  image: fern/opencode:REPLACE_WITH_FULL_COMMIT_SHA
  repo: /srv/fern/workspace
  memory: 8Gi
  env: {}
idle:
  after: 10m
proxy:
  listen: 127.0.0.1:8080
  operatorListen: 127.0.0.1:8081
  remoteOrigin: https://replace-with-this-host.example.ts.net
```

If enabled, `tasks.verification` must contain a lowercase check name,
shell-free argv beginning with an approved native host executable, relative
working directory, timeout, explicit environment, and output limit. Fern never
infers a verification command from the repository.

## 5. Install And Start systemd

```bash
sudo install -o root -g root -m 0644 deploy/systemd/fern.service \
  /etc/systemd/system/fern.service
sudo systemd-analyze verify /etc/systemd/system/fern.service
sudo systemctl daemon-reload
sudo systemctl enable --now fern.service
sudo systemctl status fern.service
sudo journalctl -u fern.service -n 100 --no-pager
```

The checked-in unit runs as `fern`, loads `/etc/fern/fern.env`, applies
`UMask=0077`, and grants Docker-group access. Do not run a second lifecycle or
offline recovery writer while the service holds the workspace lease.

Local supervision probes do not wake sleeping compute:

```bash
curl --fail http://127.0.0.1:8081/fern/live
curl --fail http://127.0.0.1:8081/fern/ready
curl --fail --user "fern:$FERN_CONTROL_PASSWORD" \
  http://127.0.0.1:8081/fern/status
curl --fail --user "fern:$FERN_CONTROL_PASSWORD" \
  http://127.0.0.1:8081/fern/metrics
```

`/fern/live` reports process liveness. `/fern/ready` returns 503 when a fixed
component is blocked or failed, while a transient `degraded` component remains
ready. `/fern/status` and `/fern/metrics` require operator auth and contain only
the fixed component registry, bounded counters, and no raw errors or dynamic
labels. Sleeping compute is healthy.

If unresolved retired publication records block readiness, stop the service,
inspect state, and run `fern debug quarantine-publications`. The command marks
records quarantined; it does not replay effects.

## 6. Publish Only Remote Ingress

```bash
sudo tailscale serve --bg http://127.0.0.1:8080
sudo tailscale serve status
sudo -H -u fern /usr/local/bin/fern doctor \
  --config /etc/fern/fern.yaml --env-file /etc/fern/fern.env --phone
```

The Serve target must be port 8080 only. The reported root HTTPS origin must
match `proxy.remoteOrigin` byte for byte. Scan the short-lived QR from the
intended browser and explicitly confirm pairing. A scanner GET preview does not
consume the code.

Automated local tests emulate HTTPS metadata over HTTP; they do not prove real
TLS redirects, WSS terminal traffic, OAuth callbacks, mobile sleep/wake, or
physical revocation. Exercise those through the actual private edge and record
redacted evidence before acceptance.

## 7. Reboot And Physical Rehearsal

On SIGINT/SIGTERM Fern writes a container-specific shutdown intent. Docker must
stop that container inside the intent window for a later stopped observation to
classify as planned. Power loss, delayed Docker shutdown, OOM, and unexplained
exit remain failed. A container Fern started but could not make healthy records
a distinct failed-start intent and also classifies failed.

Use `integration/production-rehearsal` to record, not perform, source preflight,
physical reboot, backup, source fence, replacement-host restore, TLS/WSS,
physical phone, independent ACL-negative, and final checks. The recorder's
self-test contains synthetic facts only and is not production evidence.

## Backup And Restore

Normal operators use the one-shot CLI, not the low-level archive script. Stop
the service first. Each command acquires the offline workspace lease and keeps
compute absent afterward.

```bash
sudo systemctl stop fern.service
sudo -H -u fern /usr/local/bin/fern backup create \
  --config /etc/fern/fern.yaml \
  --env-file /etc/fern/fern.env \
  --output /var/backups/fern/generation-a
```

Create destroys compute while retaining managed volumes, checkpoints SQLite,
stages Fern state/config/repository, exports the exact configured managed
volumes, and invokes the deterministic checksum/epoch archive utility. Detected
credentials and every opaque volume export go to
`generation-a.credentials.tar` by default. That file is mode `0600` but is not
encrypted by Fern; place it on approved encrypted storage or external media.
Repository files may contain secrets the utility cannot identify semantically.

Restore first verifies and stages the archive, checks that its exact volume set
matches configuration, and creates a durable pre-restore operational rollback
generation containing filesystem paths and managed volumes. It then activates
filesystem paths, validates SQLite, and replaces volumes through verified
staging volumes.

```bash
sudo -H -u fern /usr/local/bin/fern backup restore \
  --config /etc/fern/fern.yaml \
  --env-file /etc/fern/fern.env \
  --backup /var/backups/fern/generation-a
```

If post-restore validation fails:

```bash
sudo -H -u fern /usr/local/bin/fern backup rollback \
  --config /etc/fern/fern.yaml \
  --env-file /etc/fern/fern.env
```

Restore and rollback are not atomic across `/etc`, `/srv`, `/var/lib`, and the
Docker volume store. Paths and volumes activate sequentially; in-process
rollback is best effort. Abrupt power loss requires inspection and a physical
rehearsal. The retained `recovery/operational-rollback` directory deliberately
blocks another restore until the operator validates, archives, and explicitly
removes it. Never let the old and replacement hosts serve the same workspace.

## Credential Recovery

Credential bundles are separate from full host backup. Export requires absent
compute and one or more age X25519 recipients:

```bash
sudo systemctl stop fern.service
sudo -H -u fern /usr/local/bin/fern down \
  --config /etc/fern/fern.yaml
sudo -H -u fern /usr/local/bin/fern credentials export \
  --config /etc/fern/fern.yaml \
  --env-file /etc/fern/fern.env \
  --recipient age1REPLACE \
  --output /secure/fern-github-credentials.age
```

Import and rotation replace an active credential generation; they do not
bootstrap an empty App store or missing workspace-`gh` volume. They decrypt in
memory, require exact workspace/mode/hostname/
installation/repository binding, validate candidate identity and permissions
against GitHub, and write an encrypted prior-generation rollback before local
replacement. Rotation requires `--acknowledge-external-revocation` because Fern
cannot revoke the superseded key or token at GitHub. See
[Credential Recovery](./CREDENTIAL_RECOVERY.md).

## Schema Upgrade And Rollback

Current task-store schema is 6. `baseline-v1` is the first
repository-established compatibility fixture at schema 4; it is not a
historical release or tag. Before upgrade, create and verify an offline backup.

`integration/upgrade/run.sh` verifies semantic schema-4 to schema-6 upgrade,
restores the exact pre-upgrade bytes for rollback, and upgrades again. Production
rollback likewise means restoring the verified pre-upgrade backup. Older code
must not open a migrated database.

## Uninstall

Uninstall the service without deleting state:

```bash
sudo tailscale serve reset
sudo systemctl disable --now fern.service
sudo rm /etc/systemd/system/fern.service
sudo systemctl daemon-reload
sudo rm /usr/local/bin/fern
sudo docker volume inspect fern-demo-v2-data
```

This retains configuration, source, workspace, Fern state, containers, images,
and managed volumes. Delete exact resources only after verified backup and
explicit approval. Changing `workspace.name` changes resource names; never use a
generic wildcard cleanup.
