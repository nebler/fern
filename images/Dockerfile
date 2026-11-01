# syntax=docker/dockerfile:1
# Fern's images, one target each; lab/build.sh builds them:
#   fern      the daemon and the CLI, with the units install.sh installs
#   fern-box  a box: Debian slim with Postgres, git, curl, mise and Claude Code
#   fern-t3   T3 Code's server with Fern's checkpoint hook

FROM golang:1.27 AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/fern ./cmd/fern-daemon ./cmd/fern-init

# Claude Code's native binary, pinned and checked against its release manifest.
FROM debian:trixie-slim AS claude
# A failed download must fail the build, not reach the checksum's pipe.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG TARGETARCH
ARG CLAUDE_VERSION=2.1.304
ARG CLAUDE_SHA256_arm64=c487117526509ab6d2ee6013e14f181faf68d6d6afe6bb0d4b2b4a9feca5ecb3
ARG CLAUDE_SHA256_amd64=ea0f135784631619a7a409d07322ecec964f9cc4fd377508ed1ba3f141f27912
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && case "$TARGETARCH" in arm64) platform=linux-arm64 sum=$CLAUDE_SHA256_arm64 ;; \
      amd64) platform=linux-x64 sum=$CLAUDE_SHA256_amd64 ;; *) exit 1 ;; esac \
 && curl -fsSL -o /claude "https://downloads.claude.ai/claude-code-releases/$CLAUDE_VERSION/$platform/claude" \
 && echo "$sum  /claude" | sha256sum -c - \
 && chmod 755 /claude

# mise, which installs the tools a repository pins (Node, pnpm, bun, …) into
# its workspace (fern-init), pinned and checked against its release's sums.
FROM claude AS mise
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG TARGETARCH
ARG MISE_VERSION=2026.10.25
ARG MISE_SHA256_arm64=bb09e88b13164ca90691a5cc042726d079f0ba26ab55527344cb173c2e83c998
ARG MISE_SHA256_amd64=5d362ecc578dc7c146679b841dda949d6472cbf86ff5635db7afdca1b6b3fd87
RUN case "$TARGETARCH" in arm64) platform=linux-arm64 sum=$MISE_SHA256_arm64 ;; \
      amd64) platform=linux-x64 sum=$MISE_SHA256_amd64 ;; *) exit 1 ;; esac \
 && curl -fsSL -o /mise "https://github.com/jdx/mise/releases/download/v$MISE_VERSION/mise-v$MISE_VERSION-$platform" \
 && echo "$sum  /mise" | sha256sum -c - \
 && chmod 755 /mise

# GitHub's CLI, which T3 runs for pull requests, pinned and checked
# against its release's sums. images/fern-gh gives it a token.
FROM claude AS gh
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG TARGETARCH
ARG GH_VERSION=2.103.3
ARG GH_SHA256_arm64=ee32df2323c8847f5fac6cb6b4b69ca75828e9329a8f22f677021f7b3306ce63
ARG GH_SHA256_amd64=4c806978ffce1cec70853302a56d70ac6da4f19bae7adca4e1a8ab00633dd91d
RUN case "$TARGETARCH" in arm64) sum=$GH_SHA256_arm64 ;; amd64) sum=$GH_SHA256_amd64 ;; *) exit 1 ;; esac \
 && curl -fsSL -o /gh.tar.gz "https://github.com/cli/cli/releases/download/v$GH_VERSION/gh_${GH_VERSION}_linux_$TARGETARCH.tar.gz" \
 && echo "$sum  /gh.tar.gz" | sha256sum -c - \
 && tar -xzf /gh.tar.gz --strip-components=2 -C / "gh_${GH_VERSION}_linux_$TARGETARCH/bin/gh" \
 && chmod 755 /gh

FROM debian:trixie-slim AS fern
RUN apt-get update \
 && apt-get install -y --no-install-recommends btrfs-progs \
 && rm -rf /var/lib/apt/lists/*
COPY --from=go /out/fern /usr/local/bin/fern
COPY --from=go /out/fern-daemon /usr/local/bin/fern-daemon
# What install.sh installs with the binaries: the units, and the sample
# project it adds to T3.
COPY deploy/systemd /usr/share/fern/systemd
COPY lab/sample-repo /usr/share/fern/sample-repo
ENTRYPOINT ["/usr/local/bin/fern"]

FROM debian:trixie-slim AS fern-box
# Postgres binaries only: clusters live in each environment's workspace.
# Node runs the lab's toy repo, the sample without a database.
# libatomic1: the Node builds mise installs need it from Node 26 on.
# Bootstrap HTTPS before apt installs its own CA bundle: large HTTP
# downloads can stall on the lab's network.
COPY --from=claude /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources \
 && echo 'Acquire::https::CaInfo "/etc/ssl/certs/ca-certificates.crt";' > /etc/apt/apt.conf.d/fern-bootstrap-ca \
 && mkdir -p /etc/postgresql-common/createcluster.d \
 && echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.d/fern.conf \
 && apt-get update \
 && apt-get install -y --no-install-recommends postgresql-17 nodejs ca-certificates curl git tini libatomic1 \
 && rm /etc/apt/apt.conf.d/fern-bootstrap-ca \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 1000 --user-group --home-dir /tmp --no-create-home fern
COPY --from=go /out/fern-init /usr/local/bin/fern-init
COPY --from=claude /claude /usr/local/bin/claude
COPY --from=mise /mise /usr/local/bin/mise
# The root filesystem is read-only, so Claude Code must not update itself.
ENV PATH=/usr/lib/postgresql/17/bin:$PATH \
    HOME=/tmp PGHOST=/tmp PGUSER=postgres PGDATABASE=postgres \
    DISABLE_AUTOUPDATER=1
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/fern-init"]

# T3's source at the commit the patch applies to. Its root's prepare script
# sets up T3's dev tooling, which isn't installed.
FROM node:24-trixie AS t3-src
ARG T3_COMMIT=9c161462c94f7d9d6d9169da5c841457cbadf213
WORKDIR /t3
RUN git init -q && git fetch -q --depth 1 https://github.com/pingdotgg/t3code.git "$T3_COMMIT" \
 && git checkout -q FETCH_HEAD && rm -rf .git \
 && npm pkg delete scripts.prepare && corepack enable

# The server's bundle with Fern's patches, the hook's tests run first:
# fern-checkpoints.patch, the hook; fern-sessions.patch, idle provider
# sessions released after 5 minutes, so their boxes can sleep;
# fern-prewake.patch, boxes woken ahead when a thread is opened or reverted;
# fern-golden.patch, a project's golden snapshot built when it is added.
FROM t3-src AS t3-build
COPY images/t3/*.patch /tmp/
RUN git apply /tmp/fern-checkpoints.patch /tmp/fern-sessions.patch /tmp/fern-timing.patch /tmp/fern-socket.patch \
    /tmp/fern-prewake.patch /tmp/fern-golden.patch
RUN --mount=type=cache,id=fern-t3-pnpm,target=/pnpm \
    pnpm install --frozen-lockfile --store-dir /pnpm/store --filter "t3..."
WORKDIR /t3/apps/server
RUN git config --global user.email build@fern && git config --global user.name build \
 && pnpm exec vp test run src/checkpointing/FernCheckpointStore.test.ts src/checkpointing/CheckpointStore.test.ts \
      src/orchestration-v2/CheckpointRollbackService.test.ts \
 && pnpm run build:bundle

# The packages the bundle loads at run time: the server's production
# dependencies, without the web app it lists for development.
FROM t3-src AS t3-deps
RUN --mount=type=cache,id=fern-t3-pnpm,target=/pnpm \
    pnpm install --frozen-lockfile --store-dir /pnpm/store --prod --filter-prod "t3..."

FROM node:24-trixie-slim AS fern-t3
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/t3
COPY --from=t3-deps /t3/node_modules node_modules
COPY --from=t3-deps /t3/packages packages
COPY --from=t3-deps /t3/apps/server/package.json apps/server/
COPY --from=t3-deps /t3/apps/server/node_modules apps/server/node_modules
COPY --from=t3-build /t3/apps/server/dist apps/server/dist
COPY --from=go /out/fern /usr/local/bin/fern
# T3 runs "claude" from PATH, so it needs no settings: fern-claude runs it in
# the environment's box, or the real one outside environments.
COPY images/fern-claude /usr/local/bin/claude
COPY --from=claude /claude /usr/local/libexec/claude
# T3 runs "gh" for pull requests: fern-gh gives the real one a token for
# the repository from Fern's GitHub App, if there is one.
COPY images/fern-gh /usr/local/bin/gh
COPY --from=gh /gh /usr/local/libexec/gh
COPY images/t3-settings.js /opt/fern/t3-settings.js
# Where fern-t3.service binds the pool and the daemon's socket.
RUN mkdir -p /var/lib/fern /run/fern
# node's image has uid 1000 as "node"; Claude refuses full access as root.
USER 1000:1000
ENV DISABLE_AUTOUPDATER=1
ENTRYPOINT ["node", "/opt/t3/apps/server/dist/bin.mjs"]
CMD ["serve"]

# The local lab's node (lab/up.sh): Debian with systemd as PID 1, as a VPS
# runs Fern, and what the daemon runs on its node: btrfs-progs, iproute2 and
# nftables. gVisor is installed at every start, from the lab's volume.
FROM debian:trixie AS lab-node
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus btrfs-progs iproute2 nftables \
      ca-certificates curl procps psmisc util-linux kmod less python3 \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask getty.target systemd-firstboot.service systemd-logind.service console-getty.service \
      systemd-networkd-wait-online.service
COPY images/lab-node-start /usr/local/sbin/lab-node-start
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/sbin/lab-node-start"]
