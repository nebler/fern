# syntax=docker/dockerfile:1
# The Fern daemon (also the CLI and the CSI node plugin).
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/fern ./cmd/fern

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends btrfs-progs tar \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/fern /usr/local/bin/fern
# The wrapper T3 runs instead of claude; here so the lab can test it.
COPY images/fern-claude /usr/local/bin/fern-claude
ENTRYPOINT ["/usr/local/bin/fern"]
CMD ["daemon"]
