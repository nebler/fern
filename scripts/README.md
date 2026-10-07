# Repository check scripts

See the [package map](../ARCHITECTURE.md#19-package-map).

This directory holds shell checks; it contains no Go package. Release binaries
are built by `make release VERSION=vX.Y.Z` (Linux amd64/arm64, `-trimpath`,
CGO disabled, plus `dist/SHA256SUMS`). Host backup and restore are implemented
in Go by `fern backup` (`cmd/fern/backup.go`).

| Tool | Responsibility |
| --- | --- |
| `test-deployment.sh` | Validate the systemd unit and configuration examples under `deploy/systemd`. |
| `test-critical-coverage.sh` | Enforce coverage floors on the correctness-critical packages. |
