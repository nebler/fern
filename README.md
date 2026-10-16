# Fern

**Fern v2 is being redesigned.** The goal: every AI coding thread gets its own environment that behaves like git for everything (code, dependencies, the running database). It starts instantly from a golden snapshot, sleeps when idle, wakes on the next message, and rewinds or forks together with the conversation. It runs on one Linux machine or on Kubernetes, and uses your own Claude/Codex subscriptions.

- Design draft: [docs/design.md](docs/design.md)
- Fern v1 (a control plane for coding-agent jobs in disposable containers, with sealed Git artifacts) is preserved at tag [`v1-final`](https://github.com/nebler/fern/tree/v1-final).

Nothing in v2 is built yet.
