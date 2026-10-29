# Security

Fern runs code an AI agent writes, on a server, next to other threads' environments. Isolation is its job, so reports are welcome.

## Reporting a vulnerability

Report it privately on GitHub: **Security → Report a vulnerability** in this repository. Please don't open a public issue. Include what you did, what happened and, if you can, the commit you tested. You should hear back within a week.

Only the `main` branch is supported.

## What counts

The threat model is in [docs/briefs/claude-outside-the-box.md](docs/briefs/claude-outside-the-box.md). In short, these are vulnerabilities:

- code in a box reaching anything but the internet: the node, the daemon, T3's server, another box, a private address or a cloud's metadata endpoint;
- one thread's environment reading or changing another's workspace, snapshots or Claude's folder;
- Claude's login reaching a box that runs the repository's code (an environment box or a golden build), a workspace or a snapshot;
- a box changing the repository's git folder, its config or its hooks;
- anything that lets a box, or a request to the daemon's socket from outside its group, run code as root on the node.

## Running Fern safely

- T3's port (30773) must not be reachable from the internet. `lab/setup.sh` installs a firewall on a VPS that lets in only SSH; reach T3 through an SSH tunnel or a private network.
- The daemon's metrics and profiles listen on `127.0.0.1:9090`. If you point `--metrics-addr` at another address, firewall it.
- The daemon's API is the unix socket `/run/fern/fern.sock`, for root and the box user's group only.
