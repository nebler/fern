# syntax=docker/dockerfile:1
# A box: Debian slim with Postgres, git, curl and Claude Code, and fern-init as
# PID 1.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/fern-init ./cmd/fern-init

# Claude Code's native binary, pinned and checked against its release manifest.
FROM debian:trixie-slim AS claude
ARG TARGETARCH
ARG CLAUDE_VERSION=2.1.304
ARG CLAUDE_SHA256_arm64=c487117526509ab6d2ee6013e14f181faf68d6d6afe6bb0d4b2b4a9feca5ecb3
ARG CLAUDE_SHA256_amd64=ea0f135784631619a7a409d07322ecec964f9cc4fd377508ed1ba3f141f27912
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
 && case "$TARGETARCH" in arm64) platform=linux-arm64 sum=$CLAUDE_SHA256_arm64 ;; \
      amd64) platform=linux-x64 sum=$CLAUDE_SHA256_amd64 ;; *) exit 1 ;; esac \
 && curl -fsSL -o /claude "https://downloads.claude.ai/claude-code-releases/$CLAUDE_VERSION/$platform/claude" \
 && echo "$sum  /claude" | sha256sum -c - \
 && chmod 755 /claude

FROM debian:trixie-slim
# Postgres binaries only: clusters live in each environment's live/.
RUN mkdir -p /etc/postgresql-common/createcluster.d \
 && echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.d/fern.conf \
 && apt-get update \
 && apt-get install -y --no-install-recommends postgresql-17 ca-certificates curl git \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 1000 --user-group --home-dir /tmp --no-create-home fern
COPY --from=build /out/fern-init /usr/local/bin/fern-init
COPY --from=claude /claude /usr/local/bin/claude
# The root filesystem is read-only, so Claude Code must not update itself.
ENV PATH=/usr/lib/postgresql/17/bin:$PATH \
    HOME=/tmp PGHOST=/tmp PGUSER=postgres PGDATABASE=postgres \
    DISABLE_AUTOUPDATER=1
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/fern-init"]
