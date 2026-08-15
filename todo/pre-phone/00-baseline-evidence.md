# Task 00: Record The Baseline

## Commit

```text
docs: record pre-phone verification baseline
```

## Purpose

Establish one known source commit and environment before parallel branches begin. Record facts only; do not fix failures in this commit.

## Dependencies

None. Run before creating Wave 1 branches.

## Owned Files

Create only `evidence/pre-phone/baseline.md`.

## Procedure

```bash
test -z "$(gofmt -l .)"
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build ./cmd/fern
docker build -t fern/opencode:pre-phone-baseline images/opencode

git rev-parse HEAD
git status --short
go version
go env GOOS GOARCH GOTOOLCHAIN
docker version
docker info
docker image inspect fern/opencode:pre-phone-baseline
```

## Evidence Requirements

- Date, machine context and exact commit
- Command, exit status and concise output for every check
- Docker client and daemon versions separately
- Image ID and repository digest when available
- Explicit `NOT RUN` rather than inferred success
- Every blocker without repairing it

Never commit credentials, registry tokens, home-directory secrets, or full environment dumps.

## Acceptance

- Another engineer can reproduce the evidence.
- Every required command has a recorded outcome.
- No implementation or existing documentation file changed.
