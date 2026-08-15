# Task 09: Merge And Reconcile The Pre-Phone Slice

## Commit

```text
docs: integrate secure pre-phone workflow
```

## Purpose

Merge Wave 1, resolve semantic interactions, update shared entry points, and produce one coherent local-to-tailnet workflow.

## Dependencies

Tasks `01` through `08` merged and individually passing.

## Owned Files

This task may update shared integration surfaces:

```text
README.md
ROADMAP.md
ARCHITECTURE_DEEP_DIVE.md
CODEBASE_GUIDE.md
IMPLEMENTATION.md
Makefile
fern.example.yaml
.gitignore
```

Do not hide production fixes in this commit. Put any release-blocking implementation defect in a separate focused commit.

## Integration Checks

1. Configured authentication rejects unauthorized traffic before wake.
2. Valid credentials still reach OpenCode unchanged.
3. Explicit `attach -url` works with those credentials.
4. Pause error clears the endpoint and the next request reconciles.
5. Local-Docker validation happens before lifecycle mutation.
6. Version output and artifacts match deployment documentation.
7. The systemd unit references actual flags and paths.
8. The harness runs the integrated production composition.
9. CI commands work on the merged tree.

## Documentation Work

- Document versioned installation where it exists.
- Explain listener versus client origin.
- Explain pre-wake authentication accurately.
- State the local-Docker requirement.
- Link the harness and deployment runbook.
- Mark completed roadmap items and preserve deferred scope.
- Distinguish immutable deployment image identity from the development tag.
- Never claim phone success before the experiment runs.

## Full Verification

```bash
test -z "$(gofmt -l .)"
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build ./cmd/fern
docker build -t fern/opencode:pre-phone images/opencode
./scripts/test-lifecycle.sh
```

Require one green GitHub Actions run from the integrated branch.

## Acceptance

The full verification block passes locally, the integrated GitHub workflow is green, and each interaction check has a recorded result.

## Exit Gate

- Code and documentation describe the same behavior.
- No Wave 1 acceptance test regressed.
- The target host can install an identified binary and image.
- Remote usage is the remaining experiment variable, not basic operation.
