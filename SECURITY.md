# Security

Fern runs code an AI agent writes, on a server, next to other threads' environments. Isolation is its job, so reports are welcome.

## Reporting a vulnerability

Report it privately on GitHub: **Security → Report a vulnerability** in this repository. Please don't open a public issue. Include what you did, what happened and, if you can, the commit you tested. You should hear back within a week.

Only the `main` branch is supported.

## What counts

The threat model is in [docs/briefs/claude-outside-the-box.md](docs/briefs/claude-outside-the-box.md), and what is built against it, with the holes found and closed, in [architecture §13](docs/architecture.md#13-security). In short, these are vulnerabilities:

- code in a box reaching anything but the internet: the node, the daemon, T3's server, another box, a private address or a cloud's metadata endpoint;
- one thread's environment reading or changing another's workspace, snapshots or Claude's folder;
- Claude's login reaching a box that runs the repository's code (an environment box or a golden build), a workspace or a snapshot;
- a box changing the repository's git folder, its config or its hooks;
- a box getting T3's or the daemon's git, or any other process on the node, to run its code (such as through a repository it plants in the worktree), or Claude's box to run a program the box with the services wrote;
- anything that lets a box, or a request to the daemon's socket from outside its group, run code as root on the node.

## Running Fern safely

- T3's port (30773) must not be reachable from the internet. `install.sh` installs a firewall that lets in only SSH (`FERN_FIREWALL=off` keeps your own instead); reach T3 through an SSH tunnel or a private network.
- The daemon's metrics and profiles listen on `127.0.0.1:9090`. If you point `--metrics-addr` at another address, firewall it.
- The daemon's API is the unix socket `/run/fern/fern.sock`, for root and the box user's group only.
