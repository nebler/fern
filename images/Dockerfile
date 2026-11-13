# syntax=docker/dockerfile:1
# Fern's images, one target each; lab/build.sh builds them:
#   fern      the daemon and the CLI, with the units install.sh installs
#   fern-box  a box: Debian slim with Postgres, git, curl, mise, Claude Code and OpenCode
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

# OpenCode's binary, pinned and checked against its release's digests. It
# runs Claude through the claude above (its Claude Agent provider).
FROM claude AS opencode
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG OPENCODE_VERSION=2.2.6
ARG OPENCODE_SHA256_arm64=f2d27600c367e45057d6a16f6a5c2aa8d77b41a9b10d4bc3d849c2f88870e944
ARG OPENCODE_SHA256_amd64=08bb21d6326ce740d91cce7ebe88a6536933f6e1e30353b91a06b01f25da306c
RUN case "$TARGETARCH" in arm64) platform=linux-arm64 sum=$OPENCODE_SHA256_arm64 ;; \
      amd64) platform=linux-x64 sum=$OPENCODE_SHA256_amd64 ;; *) exit 1 ;; esac \
 && curl -fsSL -o /opencode.tar.gz \
      "https://github.com/sst/opencode/releases/download/v$OPENCODE_VERSION/opencode-$platform.tar.gz" \
 && echo "$sum  /opencode.tar.gz" | sha256sum -c - \
 && tar -xzf /opencode.tar.gz -C / opencode && rm /opencode.tar.gz && chmod 755 /opencode

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

# Claude's skill packs (cmd/fern-init): skills by others, each source pinned
# and checked, sorted into packs with its license. Fern's own parts of the
# packs are in images/skills.
#   cloudflare   github.com/cloudflare/skills (Apache-2.0)
#   safety       its turnstile-spin
#   design       github.com/emilkowalski/skills (MIT)
#   engineering  github.com/dmmulroy/skills and dmmulroy/anti-slop (MIT),
#                github.com/mattpocock/skills (MIT)
FROM claude AS skills
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG CLOUDFLARE_SKILLS=cbf893b954222276bf1af57e5f23379231e0efb9:6bb259020f84b8d58fdff541340d50ecaa44b4ac59a99b6cbe10bd75bbaaf67e
ARG EMIL_SKILLS=715c1807e08c497cfd1dc4feb450a2a3300ef2a9:e44d18f474e596e38866942655924158209bddbae7b8736bff0bad7911e415cd
ARG MULROY_SKILLS=8603380821fee6a77c82639f364ce8fe4f5a92be:3aec28d2de5ee9b673a1af9a783bed2b51b78a4f193a26a2d3303f04c7e5d74f
ARG ANTI_SLOP=13909c9e07590f5a85d93c62712259fe14a883f7:b0e1716fa52e3e2f9bf96a10a4a994789709b36498c10d35d3a4d4b3f9a5dced
ARG POCOCK_SKILLS=4ae2317ddb22b046e26455e4f9c9b3aaaf62212d:75b8a2ddccef7fc11059373dff29900934fc46bcb7bfdcfb034a699ce2cb0176
# fetch REPO COMMIT:SHA256 unpacks the repository at the commit into /src/REPO.
RUN fetch() { \
      curl -fsSL -o /src.tar.gz "https://codeload.github.com/$1/tar.gz/${2%%:*}" \
      && echo "${2#*:}  /src.tar.gz" | sha256sum -c - \
      && mkdir -p "/src/$1" && tar -xzf /src.tar.gz -C "/src/$1" --strip-components=1 && rm /src.tar.gz; } \
 && fetch cloudflare/skills "$CLOUDFLARE_SKILLS" && fetch emilkowalski/skills "$EMIL_SKILLS" \
 && fetch dmmulroy/skills "$MULROY_SKILLS" && fetch dmmulroy/anti-slop "$ANTI_SLOP" && fetch mattpocock/skills "$POCOCK_SKILLS" \
 && mkdir -p /skills/cloudflare /skills/safety /skills/design /skills/engineering \
 && for s in cloudflare frameworks-on-cloudflare cloudflare-app-bootstrap wrangler cf-cli workers-best-practices durable-objects web-perf; do \
      cp -r "/src/cloudflare/skills/skills/$s" /skills/cloudflare/; done \
 && cp /src/cloudflare/skills/LICENSE /skills/cloudflare/LICENSE-cloudflare-skills \
 && cp -r /src/cloudflare/skills/skills/turnstile-spin /skills/safety/ \
 && cp /src/cloudflare/skills/LICENSE /skills/safety/LICENSE-cloudflare-skills \
 && for s in animate emil-design-eng mobile-native; do \
      cp -r "/src/emilkowalski/skills/skills/$s" /skills/design/; done \
 && cp /src/emilkowalski/skills/LICENSE /skills/design/LICENSE-emilkowalski-skills \
 && cp -r /src/dmmulroy/skills/coding-standards /src/dmmulroy/anti-slop/skills/install-anti-slop /skills/engineering/ \
 && cp /src/dmmulroy/skills/LICENSE /skills/engineering/LICENSE-dmmulroy-skills \
 && cp /src/dmmulroy/anti-slop/LICENSE /skills/engineering/LICENSE-dmmulroy-anti-slop \
 && cp -r /src/mattpocock/skills/skills/engineering/codebase-design /src/mattpocock/skills/skills/engineering/diagnosing-bugs /skills/engineering/ \
 && cp /src/mattpocock/skills/LICENSE /skills/engineering/LICENSE-mattpocock-skills

FROM debian:trixie-slim AS fern
# The upgrade takes Debian's security fixes the base image doesn't have yet;
# CI's scan fails on any with a fix.
RUN apt-get update \
 && apt-get upgrade -y \
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
# xz-utils and unzip: what toolchains come packed in. A thread that needed
# LLVM first fetched xz's own package to unpack it (VPS, 2026-11-12).
# Bootstrap HTTPS before apt installs its own CA bundle: large HTTP
# downloads can stall on the lab's network. The upgrade is for Debian's
# security fixes, as in fern.
COPY --from=claude /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources \
 && echo 'Acquire::https::CaInfo "/etc/ssl/certs/ca-certificates.crt";' > /etc/apt/apt.conf.d/fern-bootstrap-ca \
 && mkdir -p /etc/postgresql-common/createcluster.d \
 && echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.d/fern.conf \
 && apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends postgresql-17 nodejs ca-certificates curl git tini libatomic1 xz-utils unzip \
 && rm /etc/apt/apt.conf.d/fern-bootstrap-ca \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 1000 --user-group --home-dir /tmp --no-create-home fern
COPY --from=go /out/fern-init /usr/local/bin/fern-init
COPY --from=claude /claude /usr/local/bin/claude
# box-opencode gives OpenCode its folders and Claude's login, in an agent box.
COPY images/box-opencode /usr/local/bin/opencode
COPY --from=opencode /opencode /usr/local/libexec/opencode
COPY --from=mise /mise /usr/local/bin/mise
COPY --from=skills /skills /usr/local/share/fern/skills
COPY images/skills /usr/local/share/fern/skills
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
# fern-golden.patch, a project's golden snapshot built when it is added;
# fern-opencode.patch, an OpenCode server per worktree, started in it;
# fern-newthread.patch, New worktree as a new project's own default, which
# T3 doesn't overrule for the first thread of a project in its projects folder;
# fern-terminals.patch, a terminal runs fern-shell, a shell in its thread's
# box, and never a shell of T3's server; T3's project scripts, which are
# typed into a terminal before it is ready for them, are left out.
FROM t3-src AS t3-build
COPY images/t3/*.patch /tmp/
RUN git apply /tmp/fern-checkpoints.patch /tmp/fern-sessions.patch /tmp/fern-timing.patch /tmp/fern-socket.patch \
    /tmp/fern-prewake.patch /tmp/fern-golden.patch /tmp/fern-opencode.patch /tmp/fern-newthread.patch \
    /tmp/fern-terminals.patch
RUN --mount=type=cache,id=fern-t3-pnpm,target=/pnpm \
    pnpm install --frozen-lockfile --store-dir /pnpm/store --filter "t3..."
WORKDIR /t3/apps/server
RUN git config --global user.email build@fern && git config --global user.name build \
 && pnpm exec vp test run src/checkpointing/FernCheckpointStore.test.ts src/checkpointing/CheckpointStore.test.ts \
      src/orchestration-v2/CheckpointRollbackService.test.ts src/project/ProjectService.test.ts \
 && pnpm exec vp test run src/terminal/Manager.test.ts -t "on a Fern server" \
 && pnpm run build:bundle

# The packages the bundle loads at run time: the server's production
# dependencies, without the web app it lists for development.
FROM t3-src AS t3-deps
RUN --mount=type=cache,id=fern-t3-pnpm,target=/pnpm \
    pnpm install --frozen-lockfile --store-dir /pnpm/store --prod --filter-prod "t3..."

# procps: T3 asks ps once a minute what its terminals run, and logged a
# warning each time it found none (VPS, 2026-11-12).
FROM node:24-trixie-slim AS fern-t3
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git procps \
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
# The same for "opencode": fern-opencode runs a worktree's server in its box.
COPY images/fern-opencode /usr/local/bin/opencode
COPY --from=opencode /opencode /usr/local/libexec/opencode
# A terminal in T3 runs fern-shell (fern-terminals.patch): a shell in the
# thread's box.
COPY images/fern-shell /usr/local/bin/fern-shell
# T3 runs "gh" for pull requests: fern-gh gives the real one a token for
# the repository from Fern's GitHub App, if there is one.
COPY images/fern-gh /usr/local/bin/gh
COPY --from=gh /gh /usr/local/libexec/gh
# T3's git, and every git that git starts, is fern-git: the real one, but
# never for a repository whose git folder a thread's box can write (a folder
# of a worktree with a .git of its own). git finds its programs in
# GIT_EXEC_PATH, which fern-t3.service sets to this folder: git's own
# programs, with fern-git as the git among them.
COPY images/fern-git /usr/local/libexec/fern-git-core/git
RUN test -x /usr/lib/git-core/git \
 && for f in /usr/lib/git-core/*; do \
      [ "$(basename "$f")" = git ] || ln -s "$f" /usr/local/libexec/fern-git-core/; \
    done \
 && ln -s /usr/local/libexec/fern-git-core/git /usr/local/bin/git
COPY images/t3-settings.js /opt/fern/t3-settings.js
# Where fern-t3.service binds the pool and the daemon's socket.
RUN mkdir -p /var/lib/fern /run/fern
# node's image has uid 1000 as "node"; Claude refuses full access as root.
USER 1000:1000
ENV DISABLE_AUTOUPDATER=1 GIT_EXEC_PATH=/usr/local/libexec/fern-git-core
ENTRYPOINT ["node", "/opt/t3/apps/server/dist/bin.mjs"]
CMD ["serve"]

# The local lab's node (lab/up.sh): Debian with systemd as PID 1, as a VPS
# runs Fern, and what the daemon runs on its node: btrfs-progs, iproute2,
# nftables and git (Claude's commits). gVisor is installed at every start,
# from the lab's volume.
FROM debian:trixie AS lab-node
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus btrfs-progs iproute2 nftables \
      ca-certificates curl procps psmisc util-linux kmod less python3 git \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask getty.target systemd-firstboot.service systemd-logind.service console-getty.service \
      systemd-networkd-wait-online.service
COPY images/lab-node-start /usr/local/sbin/lab-node-start
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/sbin/lab-node-start"]
