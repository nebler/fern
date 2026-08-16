# Supervised Private Deployment

This runbook describes a single-user Fern deployment on Ubuntu Server 24.04
with systemd, a local Docker Engine, and private Tailscale Serve. Tailscale
terminates HTTPS and forwards to Fern's loopback listener. Do not enable
Tailscale Funnel.

Fern uses one V2 contract. The browser connects to Fern's origin and receives the official
OpenCode web UI already served at `/` by `opencode2 serve`. No custom Fern coding
PWA is built or deployed.

These are source-reviewed procedures, not evidence that a complete target-host
install, reboot, backup, restore, or remote-device rehearsal has passed.

## Trust And Authentication

Fern gives OpenCode read/write access to the selected repository and forwards
provider credentials into its container. Use only a dedicated trusted host,
user, image, and repository. Docker-group membership is effectively root; this
is not tenant isolation.

The internal OpenCode credential uses username `opencode` and
`OPENCODE_PASSWORD`. Tailscale identity is the outer private-access boundary.
Basic auth is accepted for host diagnostics. For the phone demo, `fern doctor
--phone` creates a five-minute pairing link; Fern exchanges it for a secure
`HttpOnly` cookie and injects internal OpenCode auth. Pairing sessions are
process-local and must be renewed after Fern restarts. Durable device grants,
listing, revocation, and Fern administration are not implemented.

## Fast Field Demo

For a source-checkout rehearsal before installing systemd:

```bash
make image
go run ./cmd/fern init --repo /absolute/path/to/repository
# Add ANTHROPIC_API_KEY or OPENAI_API_KEY to fern.env.
go run ./cmd/fern up --config fern.yaml --env-file fern.env
```

In another terminal:

```bash
tailscale serve --bg http://127.0.0.1:8080
go run ./cmd/fern doctor --config fern.yaml --env-file fern.env --phone
```

Scan the terminal QR within five minutes. It contains a short-lived pairing
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

The protected environment file must contain a long random
`OPENCODE_PASSWORD` and only the provider keys this workspace needs. OpenCode's
username is `opencode`. Do not store these values in YAML, Git, command
arguments, or the unit file. Root and Docker administrators can inspect
container environment values.

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
```

Validate access before starting:

```bash
sudo -u fern test -r /etc/fern/fern.env -a -r /etc/fern/fern.yaml
sudo -u fern test -r /srv/fern/workspace/.git/config
sudo -u fern docker image inspect "fern/opencode:$FERN_COMMIT" >/dev/null
sudo ss -ltn '( sport = :8080 )'
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
/usr/local/bin/fern up --config /etc/fern/fern.yaml --listen 127.0.0.1:8080
```

It allows Fern time to drain proxy and manager work on SIGTERM. Stopping the
service does not intentionally stop OpenCode; use the offline `fern down`
procedure for a quiet lifecycle boundary. Do not run a second writer while the
service holds the workspace lease.

Verify the listener and V2 health route:

```bash
sudo ss -ltnp '( sport = :8080 )'
sudo bash -c 'set -a; source /etc/fern/fern.env; set +a; \
  curl --fail --user "opencode:$OPENCODE_PASSWORD" \
  http://127.0.0.1:8080/api/health'
```

`ss` must report `127.0.0.1:8080`, not a wildcard or LAN listener.

## 6. Publish With Tailscale Serve

```bash
sudo tailscale serve --bg http://127.0.0.1:8080
sudo tailscale serve status
```

From another enrolled device on a different network, open the reported HTTPS
URL. Authenticate as `opencode` with `OPENCODE_PASSWORD`; the root page must be
the official OpenCode web UI. Exercise a session and confirm that UI assets,
APIs, SSE, terminal traffic, and wake-after-idle all use the same Fern origin.
A local health request alone does not establish remote browser acceptance.

Do not run `tailscale funnel`. If Serve is unavailable, use another reviewed
private TLS reverse proxy on the same host rather than exposing Fern's HTTP
listener.

## 7. Reboot Characterization

If the host stops a running container without a Fern pause-intent record, Fern
classifies it as failed on boot rather than silently claiming a safe pause.
Automatic recovery for that state is not implemented. Back up first and treat
reboot testing as characterization:

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
- `/etc/fern` for Fern configuration and secrets.

For a quiet backup, stop the service and remove compute through Fern. `down`
retains the OpenCode volume:

```bash
export FERN_COMMIT=REPLACE_WITH_FULL_COMMIT_SHA
export BACKUP_ID="$(date -u +%Y%m%dT%H%M%SZ)"
sudo systemctl stop fern.service
sudo -H -u fern /usr/local/bin/fern down --config /etc/fern/fern.yaml
test -z "$(sudo docker ps -aq --filter name='^/demo$')"
sudo install -d -o root -g root -m 0700 "/var/backups/fern/$BACKUP_ID"
sudo tar -C /srv/fern -czf "/var/backups/fern/$BACKUP_ID/workspace.tar.gz" workspace
sudo tar -C /var/lib/fern -czf "/var/backups/fern/$BACKUP_ID/fern-state.tar.gz" .fern
sudo tar -C /etc -czf "/var/backups/fern/$BACKUP_ID/config-and-secrets.tar.gz" fern
sudo docker run --rm \
  --user 0:0 \
  -v fern-demo-v2-data:/data:ro \
  -v "/var/backups/fern/$BACKUP_ID:/backup" \
  --entrypoint tar "fern/opencode:$FERN_COMMIT" \
  -C /data -czf /backup/opencode-data.tar.gz .
sudo sh -c "cd '/var/backups/fern/$BACKUP_ID' && sha256sum *.tar.gz > SHA256SUMS"
sudo chmod -R go-rwx "/var/backups/fern/$BACKUP_ID"
sudo systemctl start fern.service
```

Repository and OpenCode archives may contain secrets; the configuration archive
does contain service credentials. Encrypt and retain them according to host
policy. Test restore into temporary paths and a temporary Docker volume rather
than overwriting live state.

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
