# Supervised Private Deployment

This runbook describes a single-user Fern deployment on Ubuntu Server 24.04
with systemd, a local Docker Engine, and private Tailscale Serve. Tailscale
terminates HTTPS and forwards to Fern's loopback listener. Do not enable
Tailscale Funnel.

Fern uses one V2 contract. The browser connects to Fern's origin and receives the official
OpenCode web UI already served at `/` by `opencode2 serve`. No custom Fern coding
PWA is built or deployed.

Static unit checks, mobile-viewport browser automation, orderly-shutdown
simulation, and an isolated checksum-verified destructive restore are automated.
A complete target-host install, physical reboot, fresh-host restore, and
remote-device rehearsal have not passed.

## Trust And Authentication

Fern gives OpenCode read/write access to the selected repository. OpenCode
owns provider connections; optional environment credentials are forwarded into
its container. Use only a dedicated trusted host,
user, image, and repository. Docker-group membership is effectively root; this
is not tenant isolation.

The internal OpenCode credential uses `opencode:$OPENCODE_PASSWORD` and is
accepted only on the loopback operator listener for local CLI access. Fern
administration and pairing issuance use the separate host-only
`fern:$FERN_CONTROL_PASSWORD` on that same operator listener; the values must
differ and the control password must contain at least 32 characters. Both Basic
credentials are rejected by the remote/device listener. Tailscale identity is
the outer private-access boundary. For the phone demo, `fern doctor --phone` creates
a five-minute pairing link. Its GET renders a confirmation page so scanner
previews do not consume the code; the confirmation POST exchanges it for a
secure `HttpOnly` cookie, and Fern injects internal OpenCode auth. The paired
device cannot administer Fern or publish. Operators use the loopback operator
origin's `/fern/control` with Fern control authentication. Hashed device grants, expiry, workflow/session
correlations, and publication records survive Fern restarts under
`/var/lib/fern/.fern/control`.

GitHub access is optional and disabled when `workspace.github` is absent.
`mode: workspace-gh` installs `gh` in the workspace and mounts its dedicated
persistent config volume at `/home/user/.config/gh`; it never reads the host
user's credential. In the OpenCode terminal, authenticate, inspect, replace, or
remove that credential with `gh auth login --hostname github.com`, `gh auth
status --hostname github.com`, and `gh auth logout --hostname github.com`.
Changes take effect on the next `gh` command without restarting Fern. Revoke a
lost credential in GitHub's application settings as well as removing it from
the workspace. The credential is unrestricted for the authenticated account;
Fern does not enforce phone-only publication or repository-only token scope.

`mode: github-app-broker` retains the repository-scoped App implementation.
Configure its positive `workspace.github.installationId` together with
repository identity and the complete `tasks` policy. If App credentials do not
exist yet, start Fern with the canonical remote HTTPS origin, open
`http://127.0.0.1:8081/fern/control` using Fern control authentication, and choose
**Connect GitHub App**. GitHub returns only to the exact private HTTPS callback;
restart Fern after success. Existing credentials disable the setup route because
rotation remains an operator procedure. The two authority modes cannot be mixed.

Fern never guesses a verification command from repository files. To enable
post-seal verification, configure `tasks.verification` with a lowercase check
name, an argument array whose first value is an absolute host executable, an
explicit repository-relative working directory, timeout, environment map, and
output byte cap. The executable must be a regular executable file, not a
script or symlink, and must satisfy Fern's immutable ownership and write-mode
policy. No shell is inserted. Its resolved path must also remain outside the
writable workspace repository. Fern binds executable content SHA-256 and fails
closed if the file changes.
Omitting this block leaves task delivery enabled but does not authorize any
verification effect.

The pinned OpenCode profile cannot automatically prove generic terminal
success. Fern therefore exposes an explicit user seal action. It previews and
revalidates an exact clean committed snapshot while the workspace is paused,
records the device/operator authorization durably, and marks the attempt
`superseded`, never `succeeded`. Idle, inactive, or empty-inbox state alone still
cannot complete a task.

## Fast Field Demo

For a source-checkout rehearsal before installing systemd:

```bash
make image
go run ./cmd/fern init --repo /absolute/path/to/repository \
  --remote-origin https://REPLACE-WITH-THIS-HOST.example.ts.net
go run ./cmd/fern up --config fern.yaml --env-file fern.env
```

In another terminal, open the official OpenCode client through Fern and use its
`/connect` flow to connect an OpenCode account or any other provider supported
by the pinned release:

```bash
go run ./cmd/fern attach --config fern.yaml --env-file fern.env
```

Replace the origin example with the exact HTTPS root that Tailscale assigns this
host before starting Fern. The connection is owned by OpenCode and stored in its persistent volume. The
web UI exposes the same provider state. Environment-key providers remain
optional.

In another terminal:

```bash
tailscale serve --bg http://127.0.0.1:8080
go run ./cmd/fern doctor --config fern.yaml --env-file fern.env --phone
```

Scan the terminal QR within five minutes in the intended browser and tap
**Pair this phone** on the confirmation page. It contains a short-lived pairing
capability, not the OpenCode password. Successful diagnostics establish private
transport and gateway readiness; they do not replace the real-phone acceptance
steps below.

## 1. Install Host Dependencies

Install Git, Go 1.24 or newer, Docker Engine, and Tailscale from their supported
Ubuntu repositories:

- <https://docs.docker.com/engine/install/ubuntu/>
- <https://tailscale.com/kb/1031/install-linux>

```bash
sudo systemctl enable --now docker tailscaled
sudo tailscale up
sudo docker version
sudo tailscale status
```

Review tailnet grants so only intended users and devices can reach this host.
Fern itself remains on loopback and needs no inbound public firewall rule.

## 2. Create The Account And Paths

| Purpose | Path |
| --- | --- |
| Trusted Fern source | `/opt/fern/src` |
| Installed binary | `/usr/local/bin/fern` |
| Configuration | `/etc/fern/fern.yaml` |
| Secrets | `/etc/fern/fern.env` |
| Service home and Fern state | `/var/lib/fern` |
| Workspace repository | `/srv/fern/workspace` |
| Backups | `/var/backups/fern` |

The service account uses UID/GID 1001 to match the workspace image. Choose a
different coordinated ID in both places if it is already allocated.

```bash
getent passwd 1001 && { echo 'UID 1001 is already allocated' >&2; exit 1; }
getent group 1001 && { echo 'GID 1001 is already allocated' >&2; exit 1; }
sudo groupadd --gid 1001 fern
sudo useradd --uid 1001 --gid 1001 --home-dir /var/lib/fern --create-home --shell /usr/sbin/nologin fern
sudo usermod -aG docker fern
sudo install -d -o root -g root -m 0755 /opt/fern
sudo install -d -o root -g fern -m 0750 /etc/fern
sudo install -d -o fern -g fern -m 0750 /var/lib/fern /srv/fern
sudo install -d -o root -g root -m 0700 /var/backups/fern
sudo -u fern git clone https://EXAMPLE.invalid/OWNER/TRUSTED-REPOSITORY.git /srv/fern/workspace
sudo -u fern git -C /srv/fern/workspace status
```

Keep Fern's source checkout and the repository controlled by OpenCode separate.

## 3. Build Exact Artifacts

Select a reviewed full commit rather than deploying a moving branch:

```bash
export FERN_REPOSITORY=https://github.com/nebler/fern.git
export FERN_COMMIT=REPLACE_WITH_FULL_COMMIT_SHA
export FERN_VERSION=v0.1.0
sudo git clone "$FERN_REPOSITORY" /opt/fern/src
sudo git -C /opt/fern/src fetch --tags --force
sudo git -C /opt/fern/src checkout --detach "$FERN_COMMIT"
test "$(sudo git -C /opt/fern/src rev-parse HEAD)" = "$FERN_COMMIT"

cd /opt/fern/src
sudo env GOTOOLCHAIN=local ./scripts/build-release.sh "$FERN_VERSION"
ARCH=$(dpkg --print-architecture)
case "$ARCH" in amd64|arm64) ;; *) echo "unsupported architecture: $ARCH" >&2; exit 1;; esac
sudo install -o root -g root -m 0755 "dist/fern-${FERN_VERSION}-linux-${ARCH}" /usr/local/bin/fern
sudo docker build --pull -t "fern/opencode:$FERN_COMMIT" images/opencode
sudo docker image inspect "fern/opencode:$FERN_COMMIT" --format '{{.Id}} {{json .RepoDigests}}'
test "$(sudo docker run --rm --entrypoint sh "fern/opencode:$FERN_COMMIT" -c 'id -u; id -g')" = "$(printf '1001\n1001')"
sha256sum /usr/local/bin/fern
/usr/local/bin/fern version
```

Record the commit, Fern version output, binary checksum, image ID, and registry
digest if one exists. A local tag is mutable; verify its image ID before each
rollout. Before production, run `make image`, `./scripts/test-lifecycle.sh`, and
`FERN_OPENCODE_IMAGE=fern/opencode:dev ./scripts/test-opencode.sh` on a suitable
build host.

## 4. Install Configuration And Secrets

```bash
cd /opt/fern/src
sudo install -o root -g fern -m 0640 deploy/systemd/fern.yaml.example /etc/fern/fern.yaml
sudo install -o root -g fern -m 0640 deploy/systemd/fern.env.example /etc/fern/fern.env
sudo sed -i "s/SOURCE_COMMIT/$FERN_COMMIT/" /etc/fern/fern.yaml
sudoedit /etc/fern/fern.env
sudoedit /etc/fern/fern.yaml
sudo chown root:fern /etc/fern/fern.env /etc/fern/fern.yaml
sudo chmod 0640 /etc/fern/fern.env /etc/fern/fern.yaml
```

The protected environment file must contain distinct long random
`OPENCODE_PASSWORD` and `FERN_CONTROL_PASSWORD` values plus only the provider
keys this workspace needs. OpenCode's username is `opencode`. Do not store these
values in YAML, Git, command arguments, or the unit file. Only the OpenCode
password and explicitly selected provider values enter Docker; root and Docker
administrators can inspect container environment values.

The deployment example deliberately contains a parseable
`https://replace-with-this-host.example.ts.net` placeholder. Replacing it with
this host's exact lowercase Tailscale HTTPS root is mandatory. Do not add a
trailing slash or explicit `:443`; Fern rejects noncanonical origins. The service
must not be started with the placeholder.

The workspace configuration selects the one image and repository; it has no
protocol selector:

```yaml
workspace:
  name: demo
  image: fern/opencode:SOURCE_COMMIT
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

Validate access before starting:

```bash
sudo -u fern test -r /etc/fern/fern.env -a -r /etc/fern/fern.yaml
sudo -u fern test -r /srv/fern/workspace/.git/config
sudo -u fern docker image inspect "fern/opencode:$FERN_COMMIT" >/dev/null
sudo ss -ltn '( sport = :8080 or sport = :8081 )'
```

The final command should show no listener.

## 5. Install And Start systemd

```bash
sudo install -o root -g root -m 0644 /opt/fern/src/deploy/systemd/fern.service /etc/systemd/system/fern.service
sudo systemd-analyze verify /etc/systemd/system/fern.service
sudo systemctl daemon-reload
sudo systemctl enable --now fern.service
sudo systemctl status fern.service
sudo journalctl -u fern.service -n 100 --no-pager
```

The unit runs:

```text
/usr/local/bin/fern up --config /etc/fern/fern.yaml --listen 127.0.0.1:8080 --operator-listen 127.0.0.1:8081
```

It allows Fern time to drain proxy and manager work on SIGTERM. Stopping the
service does not intentionally stop OpenCode; use the offline `fern down`
procedure for a quiet lifecycle boundary. Do not run a second writer while the
service holds the workspace lease.

Verify both listeners and the V2 health route through the operator surface:

```bash
sudo ss -ltnp '( sport = :8080 or sport = :8081 )'
sudo bash -c 'set -a; source /etc/fern/fern.env; set +a; \
  curl --fail --user "opencode:$OPENCODE_PASSWORD" \
  http://127.0.0.1:8081/api/health; \
  test "$(curl -sS -o /dev/null -w "%{http_code}" \
    --user "opencode:$OPENCODE_PASSWORD" \
    http://127.0.0.1:8080/api/health)" = 401'
```

`ss` must report both ports on `127.0.0.1`, never a wildcard or LAN listener.

## 6. Publish With Tailscale Serve

```bash
sudo tailscale serve --bg http://127.0.0.1:8080
sudo tailscale serve status
```

This command must target only port 8080. Never add port 8081 to Tailscale Serve.
The root HTTPS origin printed by `tailscale serve status` must be byte-for-byte
identical to `proxy.remoteOrigin`; `fern doctor --phone` also requires the same
value from local Tailscale status. Run `fern doctor --phone` locally, scan its short-lived pairing link from another
enrolled device on a different network, and complete the confirmation POST. The
root page must then be the official OpenCode web UI without a Basic prompt.
Exercise a session and confirm that UI assets, APIs, SSE, terminal traffic, and
wake-after-idle all use the same Fern origin. Confirm the backend Basic
credential receives `401` at the HTTPS origin. A local health request alone does
not establish remote browser acceptance.

The fake lifecycle and local browser tests exercise canonical HTTPS metadata
over local HTTP transport. They do not prove that the pinned OpenCode image
accepts real TLS absolute links or real WSS upgrades. Before release, verify the
exact pinned image through the actual private TLS edge: absolute `Location` and
`Link` values, UI navigation/assets, OAuth callbacks if used, SSE, and terminal
WebSocket/WSS behavior must all remain on the configured origin.

Do not run `tailscale funnel`. If Serve is unavailable, use another reviewed
private TLS reverse proxy on the same host rather than exposing Fern's HTTP
listener.

## 7. Reboot Characterization

During SIGINT or SIGTERM shutdown, Fern records a container-specific recovery
intent. Docker must stop that container within five minutes; the host may remain
offline longer before Fern resumes it. The lifecycle harness simulates this
ordering. If Docker stops first, Fern is killed forcibly, Docker shutdown starts
after the intent window, or power is lost before the intent syncs, Fern still
classifies the exit as failed. Back up first and treat a real reboot as
target-host characterization:

```bash
sudo systemctl restart fern.service
sudo reboot
```

After reconnecting, record the result:

```bash
systemctl is-enabled fern.service
systemctl is-active fern.service
sudo journalctl -b -u fern.service --no-pager
sudo tailscale serve status
sudo docker ps -a --filter name=demo
sudo docker volume inspect fern-demo-v2-data
```

Repeat authenticated local health and remote official-UI checks. Do not assume
running work or process-local provider/tool state survives a reboot.

## Data And Backup

Durable state is split across:

- `/srv/fern/workspace` for repository files and uncommitted work;
- `fern-demo-v2-data` for OpenCode sessions and configuration;
- `/var/lib/fern/.fern/state` for pause intents and `.fern/locks` for leases;
- `/var/lib/fern/.fern/control` for hashed device grants, workflow/session
  correlations, and publication operations;
- `/etc/fern` for Fern configuration and secrets.

`scripts/fern-host-backup.py` provides the host transaction foundation. It does
not stop Fern, run Docker, or contact Tailscale or GitHub. The operator must
establish a quiet boundary first. Every operation requires an operator-selected
appliance epoch and obtains an exclusive directory lock. Initialize the epoch
once per appliance; never copy its lock directory to a replacement appliance:

```bash
sudo install -d -o root -g root -m 0700 /var/lib/fern-backup-lock
sudo /opt/fern/src/scripts/fern-host-backup.py init-epoch \
  --lock-dir /var/lib/fern-backup-lock \
  --epoch appliance-2026-08
```

The lock is a fail-closed `operator.lock` directory. A process exit removes its
own lock. If power loss leaves it behind, verify that no operation is active and
remove that exact directory manually; the utility never guesses that a lock is
stale. Changing `appliance-epoch` is a replacement-appliance procedure, not a
way to bypass an old lock.

For a quiet backup, stop the service and remove compute through Fern. `down`
retains the OpenCode volume. Export each named volume into a dedicated empty
directory and supply it explicitly; the utility never enumerates Docker:

```bash
export FERN_COMMIT=REPLACE_WITH_FULL_COMMIT_SHA
export BACKUP_ID=fern-20260822-001
sudo systemctl stop fern.service
sudo -H -u fern /usr/local/bin/fern down --config /etc/fern/fern.yaml
test -z "$(sudo docker ps -aq --filter name='^/demo$')"
sudo test ! -e "/var/backups/fern/exports-$BACKUP_ID"
sudo install -d -o root -g root -m 0700 "/var/backups/fern/exports-$BACKUP_ID/opencode"
sudo docker run --rm \
  --user 0:0 \
  -v fern-demo-v2-data:/data:ro \
  -v "/var/backups/fern/exports-$BACKUP_ID/opencode:/export" \
  --entrypoint sh "fern/opencode:$FERN_COMMIT" \
  -c 'cp -a /data/. /export/'
sudo /opt/fern/src/scripts/fern-host-backup.py backup \
  --lock-dir /var/lib/fern-backup-lock \
  --epoch appliance-2026-08 \
  --generation "$BACKUP_ID" \
  --output "/var/backups/fern/$BACKUP_ID" \
  --state /var/lib/fern \
  --config /etc/fern \
  --repository /srv/fern/workspace \
  --volume "fern-demo-v2-data=/var/backups/fern/exports-$BACKUP_ID/opencode" \
  --credential-policy external \
  --credential-output "/APPROVED-ENCRYPTED-OR-EXTERNAL-MEDIA/$BACKUP_ID.credentials.tar"
sudo rm -rf "/var/backups/fern/exports-$BACKUP_ID"
sudo systemctl start fern.service
```

The general bundle contains deterministic uncompressed tar payloads, a canonical
manifest with a SHA-256 inventory for every file and directory entry, and
`SHA256SUMS` for every bundle file. Known credential paths, including Fern env
files, GitHub CLI state, GitHub App credentials, repository `.git/config`, key
files, and auth files, are
never written there. Opaque named-volume exports always require the separate
external recipient. Its tar and adjacent checksum are mode `0600`; the path must
be encrypted storage or approved external media. Path separation is not
encryption. Repository files can contain arbitrarily named secrets that no host
utility can identify semantically, so review the repository before approving
backup custody.

For a backup that deliberately omits detected credentials, use
`--credential-policy exclude` without `--credential-output`, do not supply a
volume, and plan to reauthorize GitHub, providers, and Fern service passwords.
The manifest records whether workspace `gh` credentials were externally
included, excluded for reauthorization, or not found. Backup rejects symlinks,
special files, missing roots, opaque volumes under the exclusion policy,
existing destinations, epoch mismatch, and concurrent operation.

The deterministic local release rehearsal performs byte comparison, checksum
verification, source destruction, staged restore, activation, rollback, crafted
symlink/path-escape rejection, and old-epoch rejection without real Docker:

```bash
./integration/release/run.sh
```

For an approved restore, first disable the old host to avoid two writers. On a
replacement host initialize a new appliance epoch. Install the exact recorded
Fern binary and image, place the general bundle and external recipient on local
filesystems, and restore into a dedicated generation root:

```bash
sudo systemctl stop fern.service
sudo /opt/fern/src/scripts/fern-host-backup.py restore \
  --lock-dir /var/lib/fern-backup-lock \
  --epoch replacement-appliance-2026-08 \
  --backup "/var/backups/fern/$BACKUP_ID" \
  --credential-input "/APPROVED-ENCRYPTED-OR-EXTERNAL-MEDIA/$BACKUP_ID.credentials.tar" \
  --target /var/lib/fern-restore-generations
sudo test -f /var/lib/fern-restore-generations/current/.fern-appliance-epoch
sudo test "$(cat /var/lib/fern-restore-generations/current/.fern-appliance-epoch)" = replacement-appliance-2026-08
```

Restore verifies both layers of checksums before writing, rejects links, special
entries, duplicate names and absolute or `..` paths, writes a staging generation,
then renames it to `current`. An existing `current` becomes `previous`; only one
previous generation is retained. Because `/etc`, `/srv`, `/var/lib`, and Docker
volumes are separate activation domains, the utility cannot atomically replace
the live host layout. With the service stopped, inspect `current`, verify UID/GID
policy, then install its `config`, `repository`, and `state` trees into empty live
destinations and import `current/volumes/fern-demo-v2-data` into a newly created
volume using the pinned image. Do not overlay an existing live tree or volume.
The utility writes `TRANSACTION-MANIFEST.json` beside `current` after each
successful restore or rollback; its active generation and rollback availability
are observations from that completed operation, not a static release example.

The volume creation/import step remains host-specific:

```bash
if sudo docker volume inspect fern-demo-v2-data >/dev/null 2>&1; then
  echo 'refusing to merge restore into existing volume fern-demo-v2-data' >&2
  exit 1
fi
sudo docker volume create \
  --label dev.fern.managed=true \
  --label dev.fern.workspace=demo \
  fern-demo-v2-data
sudo docker run --rm --user 0:0 \
  -v fern-demo-v2-data:/data \
  -v /var/lib/fern-restore-generations/current/volumes/fern-demo-v2-data:/restore:ro \
  --entrypoint sh "fern/opencode:$FERN_COMMIT" \
  -c 'test -z "$(find /data -mindepth 1 -print -quit)" && cp -a /restore/. /data/'
sudo systemctl start fern.service
sudo -H -u fern /usr/local/bin/fern doctor \
  --config /etc/fern/fern.yaml \
  --env-file /etc/fern/fern.env
```

Before live installation, rollback within the generation root is executable as:

```bash
sudo /opt/fern/src/scripts/fern-host-backup.py rollback \
  --lock-dir /var/lib/fern-backup-lock \
  --epoch replacement-appliance-2026-08 \
  --target /var/lib/fern-restore-generations
```

Rollback swaps `current` and `previous` only when both carry the selected active
epoch. It does not reverse a Docker import or live cross-filesystem copies. A
physical-host rehearsal must additionally test service quiescence, free space,
permissions and ownership, filesystem rename/crash behavior, Docker volume
export/import, Tailscale reauthorization, GitHub/provider reauthorization when
excluded, and authenticated application health before acceptance.

## Uninstall And Explicit Deletion

Uninstalling the service should preserve data:

```bash
sudo tailscale serve status
# On a host dedicated to this mapping only:
sudo tailscale serve reset
sudo systemctl disable --now fern.service
sudo rm /etc/systemd/system/fern.service
sudo systemctl daemon-reload
sudo systemctl reset-failed fern.service
sudo rm /usr/local/bin/fern
sudo docker volume inspect fern-demo-v2-data
```

This retains configuration, source, workspace files, Fern state, containers,
images, and `fern-demo-v2-data`.

Only after a verified backup and explicit approval, inspect and remove the exact
resources:

```bash
sudo docker inspect demo
sudo docker rm -f demo
sudo docker volume rm fern-demo-v2-data
sudo docker image rm "fern/opencode:REPLACE_WITH_FULL_COMMIT_SHA"
sudo rm -rf /srv/fern/workspace /var/lib/fern/.fern /etc/fern /opt/fern/src
sudo userdel fern
```

Do not run the deletion block as ordinary uninstall. Verify names against the
deployed configuration; changing `workspace.name` changes resource names.
