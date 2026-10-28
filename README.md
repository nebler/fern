# Fern

**Fern v2 is being redesigned.** The goal: every AI coding thread gets its own environment that behaves like git for everything (code, dependencies, the running database). It starts instantly from a golden snapshot, sleeps when idle, wakes on the next message, and rewinds or forks together with the conversation. It runs on a remote Linux machine as two systemd services (an existing Kubernetes cluster works too), and uses your own Claude/Codex subscriptions. Your Mac or phone only runs the T3 app.

- Architecture (what we're building): [docs/architecture.md](docs/architecture.md)
- Roadmap (local first; MVP in ~3.5 weeks, v1 in ~8): [docs/roadmap.md](docs/roadmap.md)
- Engine experiments and dev loop: [docs/engine-local.md](docs/engine-local.md)
- Design draft (research, landscape, decisions): [docs/design.md](docs/design.md)
- Fern v1 (a control plane for coding-agent jobs in disposable containers, with sealed Git artifacts) is preserved at tag [`v1-final`](https://github.com/nebler/fern/tree/v1-final).

Built so far (iteration 2): the daemon with its btrfs store, CSI plugin and Kubernetes driver; boxes with services, exec and Claude Code; T3's server on the node with a checkpoint hook that snapshots each turn's whole disk. Everything runs and is tested in a local lab (a systemd node in Docker) and on a VPS; see [lab/RESULTS.md](lab/RESULTS.md).
