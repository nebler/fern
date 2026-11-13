# Running a Fern server: what to do when something is wrong

For the owner of one server installed with `install.sh`. Commands marked *server* run there as root (`ssh root@HOST`); the others run on your Mac. Why things work as they do: [architecture](architecture.md) §12 (failures), §14 (running), §16 (observability).

## See what is wrong

| Command | Where | What it says |
|---|---|---|
| `fern doctor` | server | every part of the node, ok or not, and what the daemon knows has gone wrong, each with what to do. Start here. |
| `bash install.sh doctor root@HOST` | Mac | the same, and whether your Mac's tunnel reaches T3 |
| `fern top` | server | the boxes that are up, their memory and CPU, what holds them awake; the pool's room; the same problems |
| `fern logs <env>` | server | what the daemon said of one environment, and what its boxes printed (also of the last run of a box that stopped). `fern ls` lists the environments; a thread's is `t3-…` |
| `fern df` | server | what uses the pool and the disk under it, and what `fern gc` would free |
| `journalctl -u fern-daemon -n 100`, `journalctl -u fern-t3 -n 100` | server | the daemon's and T3's own logs |

With `FERN_NOTIFY_URL` set (below), the problems `fern doctor` lists come to your phone, and so does a daemon or T3 that systemd gives up on; with `FERN_HEARTBEAT_URL`, a watcher tells you when the server goes quiet.

## When something is wrong, in order

**The pool or the disk is full** (`disk`: no new snapshots or threads)
1. `fern df` — is it the pool, or the server's disk under it?
2. `fern gc` — frees old images, what image builds left, snapshots past their retention.
3. Delete threads that are done, in T3.
4. The pool: `fern pool grow +10G` (with boxes running), if the disk under it has the room (`fern df`). The server's disk: `journalctl --vacuum-size=200M`; a larger disk from your provider, then `fern pool grow`.

**The pool's records are full** (`pool records`: btrfs has no room left for its own records; writes fail with free space showing)
1. `fern pool grow +10G`. The problem stays listed for up to ten minutes after, as the daemon reads btrfs that often.
2. With no disk to grow into: delete threads that are done, then `btrfs balance start -dusage=50 /var/lib/fern` (server; it packs half-empty data chunks to give btrfs room back, and takes minutes of disk work).

**A box keeps crashing** (`box`: it keeps crashing, and its commands fail until a time it names)
1. `fern logs <env>` — the box's last output says why (out of memory, a service that exits).
2. `fern top` — is the server short of memory?
3. Mend the cause (often the repository's `services.yaml`, or `--box-memory` in `/etc/fern/daemon.env`, then `systemctl restart fern-daemon`), then `fern wake <box>` to try at once.

**A golden build fails** (`golden build`: new threads start without what the setup script makes)
1. `fern doctor` shows the first line; the whole failure, with what the script printed, is `/var/lib/fern/envs/<builder>/build-failed.json`.
2. Mend the repository's setup script or `services.yaml` and push. The first new thread 15 minutes after the failure builds again; one that works clears the problem. Threads meanwhile work, only slower to start.

**A turn's snapshot failed** (`snapshot`: a revert to that turn is refused)
1. `fern logs <env>` says why. Disk and pool records first, above.
2. `fern snapshot <env> probe` tries one now; if it works the problem clears; `fern rmsnap <env> probe` removes it again.

**T3 doesn't answer** (the app can't connect)
1. `bash install.sh doctor root@HOST` — the tunnel, or the server?
2. Server: `systemctl status fern-t3`, `journalctl -u fern-t3 -n 50`.
3. `systemctl restart fern-t3` (a turn that runs is cut off; T3 can continue it).
4. The tunnel: run the install again from your Mac; it sets the tunnel up again.

**The daemon doesn't start**
1. `systemctl status fern-daemon`, `journalctl -u fern-daemon -n 50`: the last lines say why.
2. "is not on btrfs": the pool isn't mounted: `mount /var/lib/fern`, then `systemctl restart fern-daemon fern-t3`.
3. "flag provided but not defined": a flag in `/etc/fern/daemon.env` this daemon doesn't know; take it out.
4. After an update: roll back (below).
5. Otherwise run the install again: it puts back every file it owns.

**After a reboot or a power cut** nothing needs doing. The firewall, the pool (fstab), the daemon and T3 start by themselves; the daemon finishes or clears what a change left half-way (`Reconcile`). Boxes don't survive a reboot: each wakes at its next command. A turn that ran is cut off, and T3 can continue it. A power cut can lose the last seconds of writes, and the last record of a failed build or snapshot. `fern doctor` after, to be sure. `fern doctor` says when the system asks for a reboot; do it when no turn runs (`fern top`): `systemctl reboot`.

**An update went wrong:** `bash install.sh rollback root@HOST` (or `bash install.sh rollback` on the server, as root). It puts back the binaries, units and images the last update replaced and restarts the daemon, then T3 once running turns have ended (up to `FERN_DRAIN`, 600 s). Running the install again brings the newer release back. A rollback can't undo:
- what the newer T3 did to its own database (`/var/lib/fern/userdata`): an older T3 may not read a database a newer one has migrated. Not checked for each release.
- what the threads did meanwhile: the pool isn't rolled back, only the software.
- a flag in `FERN_DAEMON_ARGS` the older daemon doesn't know: the rollback takes it out (`daemon.env.before-rollback` has the file before).
- what the newer daemon wrote that the older doesn't know, such as a failed snapshot's record: the older ignores it, and it stays until the newer one runs again.
- one release back only, and only to one `install.sh` installed (not one `lab/build.sh` did).

**The server is lost**
- With backups: a new server, then `FERN_RESTORE_FROM=<old hostname>` and the same `FERN_BACKUP_*` settings with the install (`curl … | bash -s -- root@NEW`). It runs `fern-daemon restore`, which brings back each thread's workspace as of its last backup (at most an hour old), its record, T3's data and the projects' repositories. Your Mac must still have `~/.config/fern/backup-key`.
- Without backups: everything on it is gone: every thread, its workspace, its snapshots, its database and the projects' repositories. What survives is what was pushed to GitHub. `fern doctor` says how much is at stake (`backups`).

**Claude's login has expired** (Claude in a thread says it isn't logged in)
1. Server: `rm /etc/fern/claude.env`.
2. Mac: run the install again; it asks for a new token (`claude setup-token`) and restarts T3. Boxes read the login at every wake.

## Routine

- **Update:** run the install again from your Mac: `curl -fsSL https://raw.githubusercontent.com/nebler/fern/main/install.sh | bash -s -- root@HOST` (`FERN_VERSION=<tag>` for a given release). It waits up to ten minutes for running turns, and keeps the release it replaces for a rollback.
- **Grow the pool:** `fern pool grow +10G` or `fern pool grow 70G` (server), with boxes running; safe to run again.
- **Backups:** the install with `FERN_BACKUP_ENDPOINT`, `_REGION`, `_BUCKET`, `_ACCESS_KEY`, `_SECRET_KEY` ([README](../README.md#try-it)); `fern backup` makes one now (server). Keep a copy of `~/.config/fern/backup-key`.
- **Notifications:** the install with `FERN_NOTIFY_URL=https://ntfy.sh/<a topic nobody would guess>` (subscribe to it in the ntfy app), and `FERN_HEARTBEAT_URL=<a check's ping URL>` from healthchecks.io or another dead man's switch, with a period of 5 minutes and some grace. `off` turns either off. `curl -d test https://ntfy.sh/<topic>` checks your phone's end.
