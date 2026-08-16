# Supervised Private Deployment

This runbook defines a single-user Fern deployment using systemd, a local Docker daemon, and private Tailscale Serve. Fern stays on loopback. Tailscale terminates HTTPS and makes it available only inside the tailnet. **Do not enable Tailscale Funnel.** Fern does not provide TLS itself.

The reference target is Ubuntu Server 24.04 LTS with systemd, Docker Engine, and Tailscale. The files and commands are source-reviewed examples; this repository does not claim that the complete install, remote access, reboot, backup, or restore sequence has been executed. Record those results separately during the target-host rehearsal.

The reference configuration deliberately uses OpenCode V1. V1 remains the
recommended phone-test deployment. The pinned V2 beta is an explicit opt-in;
follow [OPENCODE_V2.md](./OPENCODE_V2.md), use the separate V2 image and volume,
and rerun its smoke tests on the target host before changing this runbook's
examples.

## Trust And Access Model

Fern gives the workspace container read/write access to the selected host repository and forwards provider credentials into it. OpenCode can run tools and modify that repository. Only use a repository whose code, hooks, configuration, and collaborators are trusted to receive those credentials and act as the host user. This is a dedicated trusted-user/trusted-repository deployment, not tenant isolation.

Membership in the `docker` group is effectively root access. This runbook assumes Fern talks to the local rootful Docker Engine through `/var/run/docker.sock`; do not set `DOCKER_HOST` to a remote daemon. Tailscale identity and tailnet policy are the outer access boundary. OpenCode Basic authentication remains enabled as defense in depth.

## 1. Install Host Dependencies

Install Docker Engine from Docker's supported Ubuntu repository and Tailscale from Tailscale's supported Ubuntu repository, rather than an unreviewed convenience script. Follow the current vendor instructions:

- <https://docs.docker.com/engine/install/ubuntu/>
- <https://tailscale.com/kb/1031/install-linux>

Then enroll the host in the intended tailnet and confirm the local services:

```bash
sudo systemctl enable --now docker tailscaled
sudo tailscale up
sudo docker version
sudo tailscale status
```

Review tailnet grants so only intended users/devices can reach this host. No inbound public firewall rule is needed for Fern's port because it remains on loopback.

## 2. Create The Account And Paths

The paths are deliberately separate:

| Purpose | Path |
| --- | --- |
| Trusted source checkout | `/opt/fern/src` |
| Installed binary | `/usr/local/bin/fern` |
| Configuration | `/etc/fern/fern.yaml` |
| Secrets | `/etc/fern/fern.env` |
| Service home and working directory | `/var/lib/fern` |
| Fern lock and pause-intent state | `/var/lib/fern/.fern` |
| Checked-out workspace | `/srv/fern/workspace` |
| Backups | `/var/backups/fern` |

Create a non-login service account and directories. The host account uses the
same fixed UID/GID as the image so OpenCode can write the bind-mounted
repository on Linux. Stop and choose another coordinated ID in both the image
and these commands if either ID is already allocated. Adding the account to
`docker` is necessary for the current local-Docker implementation and carries
the root-equivalent warning above.

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
```

Clone the repository Fern will control into `/srv/fern/workspace` as `fern`. Do not point Fern at `/opt/fern/src`; keep product source and the controlled workspace distinct.

```bash
sudo -u fern git clone https://EXAMPLE.invalid/OWNER/TRUSTED-REPOSITORY.git /srv/fern/workspace
sudo -u fern git -C /srv/fern/workspace status
```

Replace the placeholder URL with the reviewed repository origin. Confirm ownership with `sudo -u fern test -r /srv/fern/workspace/.git/config`.

## 3. Install An Exact Fern Build And Image

Choose a reviewed commit, clone Fern as root-owned source, and verify that checkout before building. Do not deploy from a moving branch name.

```bash
export FERN_REPOSITORY=https://github.com/nebler/fern.git
export FERN_COMMIT=REPLACE_WITH_FULL_COMMIT_SHA
sudo git clone "$FERN_REPOSITORY" /opt/fern/src
sudo git -C /opt/fern/src fetch --tags --force
sudo git -C /opt/fern/src checkout --detach "$FERN_COMMIT"
test "$(sudo git -C /opt/fern/src rev-parse HEAD)" = "$FERN_COMMIT"

cd /opt/fern/src
sudo env GOTOOLCHAIN=local go build -trimpath -o /usr/local/bin/fern ./cmd/fern
sudo chown root:root /usr/local/bin/fern
sudo chmod 0755 /usr/local/bin/fern
sudo docker build --pull -t "fern/opencode:$FERN_COMMIT" images/opencode
sudo docker image inspect "fern/opencode:$FERN_COMMIT" --format '{{.Id}} {{json .RepoDigests}}'
test "$(sudo docker run --rm --entrypoint sh "fern/opencode:$FERN_COMMIT" -c 'id -u; id -g')" = "$(printf '1001\n1001')"
sha256sum /usr/local/bin/fern
```

This source build requires Go 1.24 or newer. Save the full commit, binary checksum, image ID, and any repository digest in the rehearsal record. A local image ID identifies the built image on this daemon; a registry digest is available only if the image is pushed to or pulled from a registry.

## 4. Install Configuration And Secrets

Install the examples, replace `SOURCE_COMMIT` with the same full commit used in the image tag, and review every value:

```bash
cd /opt/fern/src
sudo install -o root -g fern -m 0640 deploy/systemd/fern.yaml.example /etc/fern/fern.yaml
sudo install -o root -g fern -m 0640 deploy/systemd/fern.env.example /etc/fern/fern.env
sudo sed -i "s/SOURCE_COMMIT/$FERN_COMMIT/" /etc/fern/fern.yaml
sudoedit /etc/fern/fern.env
sudoedit /etc/fern/fern.yaml
sudo chmod 0640 /etc/fern/fern.env /etc/fern/fern.yaml
sudo chown root:fern /etc/fern/fern.env /etc/fern/fern.yaml
```

Generate a password without placing it in shell history, for example by generating it inside `sudoedit` or a root-only shell. The environment file must contain a non-empty `OPENCODE_SERVER_PASSWORD`. Keep provider keys and the OpenCode password out of YAML, command arguments, Git, and the unit file. Be aware that root and processes with Docker access can still inspect container environment values.

Validate access and the pinned image before starting:

```bash
sudo -u fern test -r /etc/fern/fern.env -a -r /etc/fern/fern.yaml
sudo -u fern test -r /srv/fern/workspace/.git/config
sudo -u fern docker image inspect "fern/opencode:$FERN_COMMIT" >/dev/null
sudo ss -ltn '( sport = :8080 )'
```

The last command should show no existing listener.

## 5. Install And Start The Unit

```bash
sudo install -o root -g root -m 0644 /opt/fern/src/deploy/systemd/fern.service /etc/systemd/system/fern.service
sudo systemd-analyze verify /etc/systemd/system/fern.service
sudo systemctl daemon-reload
sudo systemctl enable --now fern.service
sudo systemctl status fern.service
sudo journalctl -u fern.service -n 100 --no-pager
sudo journalctl -u fern.service -f
```

The unit runs the current CLI form:

```text
/usr/local/bin/fern up --config /etc/fern/fern.yaml --listen 127.0.0.1:8080
```

It sends SIGTERM on stop and allows 120 seconds for Fern's graceful proxy, watcher, and in-flight lifecycle shutdown before systemd may kill it. `Restart=on-failure` with a delay and start limit recovers crashes without a tight restart loop. An ordinary `systemctl stop` is not restarted.

Verify that Fern is loopback-only and authenticate to the local endpoint:

```bash
sudo ss -ltnp '( sport = :8080 )'
sudo bash -c 'set -a; source /etc/fern/fern.env; set +a; \
  curl --fail --config <(printf "user = \"%s:%s\"\n" \
  "$OPENCODE_SERVER_USERNAME" "$OPENCODE_SERVER_PASSWORD") \
  http://127.0.0.1:8080/global/health'
```

`ss` must report `127.0.0.1:8080`, not `0.0.0.0:8080`, `[::]:8080`, or a host LAN address.

Service operations are:

```bash
sudo systemctl start fern.service
sudo systemctl stop fern.service
sudo systemctl restart fern.service
sudo systemctl status fern.service
sudo journalctl -u fern.service --since today
```

Do not run a second `fern up`, `fern down`, or `fern resume` while the service owns the workspace lease. Stop the service first for offline lifecycle commands.

## 6. Publish Privately With Tailscale Serve

Publish the loopback backend through private Serve:

```bash
sudo tailscale serve --bg http://127.0.0.1:8080
sudo tailscale serve status
sudo tailscale status
```

Use the HTTPS tailnet URL printed by `tailscale serve status`. From another enrolled device on a different network, authenticate with the configured OpenCode username and password and request `/global/health`, then test the actual OpenCode client flow. A successful local request does not prove tailnet access.

Tailscale Serve provides TLS and tailnet identity at the edge; its loopback hop to Fern is plain HTTP on the same host. **Do not run `tailscale funnel` or pass a Funnel flag.** Funnel is public internet exposure and is outside this deployment.

Tailscale also permits direct tailnet publication by binding Fern to the host's Tailscale IP. That is a different topology: it is not loopback-only, exposes Fern's plain HTTP listener directly to permitted tailnet peers, and requires changing the unit/config whenever the address changes. This runbook intentionally does not use it. If Serve cannot be used, treat direct binding as a separately reviewed deployment, require the OpenCode password, bind only to `tailscale ip -4` rather than `0.0.0.0`, and use an `http://TAILSCALE_IP:PORT` URL because Fern itself has no TLS.

## 7. Reboot And Shutdown Checks

Run these on the target host; they are acceptance steps, not claims of results:

```bash
sudo systemctl restart fern.service
sudo systemctl stop fern.service
sudo journalctl -u fern.service -n 100 --no-pager
sudo systemctl start fern.service
sudo reboot
```

After reconnecting, record rather than assume the outcome:

```bash
systemctl is-enabled fern.service
systemctl is-active fern.service
sudo journalctl -b -u fern.service --no-pager
sudo tailscale serve status
sudo docker ps -a --filter name=fern-demo
sudo docker volume inspect fern-demo-data
```

Repeat local authenticated health and remote-tailnet client tests. Confirm that SIGTERM shutdown did not end in `stop-sigterm timed out` or a forced `SIGKILL`. Record whether the named volume, OpenCode sessions, and intended paused/stopped state survived restart and reboot.

## Data Ownership And Retention

Fern intentionally spreads durable state across three places:

- `/srv/fern/workspace` contains repository files and uncommitted work through a host bind mount.
- Docker volume `fern-demo-data` contains OpenCode session data. `fern down` removes compute but retains this volume.
- `/var/lib/fern/.fern/state` contains pause-intent records used to distinguish intentional stop from failure; `/var/lib/fern/.fern/locks` contains runtime lock files.

The container and local `fern/opencode:<commit>` image are replaceable compute, but retain them during incident investigation. Configuration and secrets live under `/etc/fern`. Tailscale Serve configuration belongs to Tailscale, not Fern. Neither stopping nor uninstalling the systemd unit deletes any of these data stores.

## Backup Before Destructive Tests

Stop Fern first so the repository, session volume, and pause-intent state form a quiet snapshot. These commands use the already pinned Fern image rather than pulling an extra backup image.

```bash
export FERN_COMMIT=REPLACE_WITH_FULL_COMMIT_SHA
export BACKUP_ID="$(date -u +%Y%m%dT%H%M%SZ)"
sudo systemctl stop fern.service
sudo install -d -o root -g root -m 0700 "/var/backups/fern/$BACKUP_ID"
sudo git -C /srv/fern/workspace status --short >"/tmp/fern-status-$BACKUP_ID"
sudo tar -C /srv/fern -czf "/var/backups/fern/$BACKUP_ID/workspace.tar.gz" workspace
sudo tar -C /var/lib/fern -czf "/var/backups/fern/$BACKUP_ID/fern-state.tar.gz" .fern
sudo tar -C /etc -czf "/var/backups/fern/$BACKUP_ID/config-and-secrets.tar.gz" fern
sudo docker run --rm \
  --user 0:0 \
  -v fern-demo-data:/data:ro \
  -v "/var/backups/fern/$BACKUP_ID:/backup" \
  --entrypoint tar "fern/opencode:$FERN_COMMIT" \
  -C /data -czf /backup/opencode-data.tar.gz .
sudo mv "/tmp/fern-status-$BACKUP_ID" "/var/backups/fern/$BACKUP_ID/git-status.txt"
sudo sh -c "cd '/var/backups/fern/$BACKUP_ID' && sha256sum *.tar.gz > SHA256SUMS"
sudo chmod -R go-rwx "/var/backups/fern/$BACKUP_ID"
sudo systemctl start fern.service
```

The workspace archive can contain secrets and the config archive does contain service credentials. Protect and encrypt backups according to host policy. Before a destructive rehearsal, test restoration into temporary directories and a temporary Docker volume; do not overwrite live data merely to test the procedure.

Example volume validation:

```bash
sudo docker volume create fern-restore-check
sudo docker run --rm \
  --user 0:0 \
  -v fern-restore-check:/data \
  -v "/var/backups/fern/$BACKUP_ID:/backup:ro" \
  --entrypoint tar "fern/opencode:$FERN_COMMIT" \
  -C /data -xzf /backup/opencode-data.tar.gz
sudo docker run --rm -v fern-restore-check:/data \
  --entrypoint sh "fern/opencode:$FERN_COMMIT" -c 'test -d /data && find /data -maxdepth 2 -print | head'
sudo docker volume rm fern-restore-check
```

## Uninstall Without Deleting Data

First inspect Tailscale's mappings. On a host dedicated to this one Serve mapping, `reset` removes it; do not reset a shared Serve configuration.

```bash
sudo tailscale serve status
# Dedicated mapping only; this clears all Serve mappings on the host:
sudo tailscale serve reset

sudo systemctl disable --now fern.service
sudo rm /etc/systemd/system/fern.service
sudo systemctl daemon-reload
sudo systemctl reset-failed fern.service
sudo rm /usr/local/bin/fern
```

This uninstall retains `/etc/fern`, `/var/lib/fern`, `/srv/fern/workspace`, `/opt/fern/src`, all Fern containers/images, and `fern-demo-data`. It also leaves Docker, Tailscale, and the `fern` account installed. Confirm retention:

```bash
sudo test -d /srv/fern/workspace -a -d /var/lib/fern/.fern
sudo docker volume inspect fern-demo-data
sudo docker ps -a --filter name=fern-demo
```

## Explicit Data Deletion

Only after a verified backup and explicit approval, remove retained data. Stop and uninstall the unit first. The ordinary uninstall may already have removed the Fern binary, so remove the known container explicitly before its volume. Verify the names against the configuration and `docker inspect` first:

```bash
sudo docker inspect fern-demo
sudo docker rm -f fern-demo
sudo docker volume rm fern-demo-data
sudo docker image rm "fern/opencode:REPLACE_WITH_FULL_COMMIT_SHA"
sudo rm -rf /srv/fern/workspace /var/lib/fern/.fern /etc/fern /opt/fern/src
sudo userdel fern
```

Do not run the final block as part of ordinary uninstall. Review each path and volume name against `/etc/fern/fern.yaml`; changing `workspace.name` changes resource names.
