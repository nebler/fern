# Task 04: Reject Unsupported Remote Docker

## Commit

```text
fix(cli): reject unsupported remote Docker endpoints
```

## Purpose

Make Fern's topology assumption explicit before mutation. Repository bind paths, backend loopback, host locks, and pause intent assume Fern and Docker run on one machine.

## Dependencies

Start from task `00`. Independent of all other Wave 1 tasks.

## Owned Files

May modify `cmd/fern/helpers.go` and create `cmd/fern/docker_topology_test.go`.

Do not edit runtime implementation files or shared documentation.

## Contract

- Allow default local Docker configuration.
- Allow explicit local Unix-socket endpoints used by supported hosts.
- Reject TCP, HTTP, HTTPS, SSH, and other clearly remote endpoints.
- Explain that local Docker is required for bind mounts, loopback publication, and host-local coordination.
- Fail before creating, starting, stopping, or removing Docker resources.
- Use a robust Docker host parser rather than fragile prefix checks when available.

Do not claim Windows support merely because Docker recognizes `npipe`.

## Tests

Table-test unset host, local Unix sockets, TCP, HTTP, HTTPS, SSH, malformed values, and misleading local-looking hostnames. Tests must not contact Docker.

## Acceptance

```bash
GOTOOLCHAIN=local go test ./cmd/fern
GOTOOLCHAIN=local go test -race ./cmd/fern
```

## Out Of Scope

Remote repository synchronization, distributed locks, tunnels, remote intent storage, and multi-host Docker contexts.
