# syntax=docker/dockerfile:1
# T3 Code's server with Fern's checkpoint hook (images/t3/fern-checkpoints.patch),
# the fern CLI, and fern-claude in place of claude. lab/build-t3.sh builds it:
# the "fern" and "fern-box" contexts are the lab images, which supply the CLI
# and the pinned Claude Code.

# T3's source at the commit the patch applies to. A shallow fetch: T3's
# submodules are vendored references the server doesn't build from.
FROM node:24-trixie AS t3
ARG T3_COMMIT=9c161462c94f7d9d6d9169da5c841457cbadf213
WORKDIR /t3
RUN git init -q && git fetch -q --depth 1 https://github.com/pingdotgg/t3code.git "$T3_COMMIT" \
 && git checkout -q FETCH_HEAD && rm -rf .git

# The patched bundle; the hook's tests run against it before anything ships.
FROM node:24-trixie AS build
WORKDIR /src
COPY --from=t3 /t3 ./
COPY images/t3/fern-checkpoints.patch /tmp/
# The root's prepare script sets up T3's dev tooling, which isn't installed.
RUN git apply /tmp/fern-checkpoints.patch && npm pkg delete scripts.prepare && corepack enable
RUN --mount=type=cache,id=fern-t3-pnpm,target=/pnpm \
    pnpm install --frozen-lockfile --store-dir /pnpm/store --filter "t3..."
WORKDIR /src/apps/server
RUN git config --global user.email build@fern && git config --global user.name build \
 && pnpm exec vp test run src/checkpointing/FernCheckpointStore.test.ts src/checkpointing/CheckpointStore.test.ts \
 && pnpm run build:bundle

# Only what the bundle loads at run time: its external packages.
FROM node:24-trixie AS deps
WORKDIR /opt/t3
COPY --from=t3 /t3 ./
RUN npm pkg delete scripts.prepare && corepack enable
RUN --mount=type=cache,id=fern-t3-pnpm,target=/pnpm \
    pnpm install --frozen-lockfile --store-dir /pnpm/store --prod --filter "t3..."
COPY --from=build /src/apps/server/dist apps/server/dist

FROM node:24-trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git \
 && rm -rf /var/lib/apt/lists/*
COPY --from=deps /opt/t3 /opt/t3
COPY --from=fern /usr/local/bin/fern /usr/local/bin/fern
# T3 runs "claude" from PATH, so it needs no settings: fern-claude runs it in
# the environment's box, or the real one outside environments.
COPY --from=fern /usr/local/bin/fern-claude /usr/local/bin/claude
COPY --from=fern-box /usr/local/bin/claude /usr/local/libexec/claude
# node's image has uid 1000 as "node"; Claude refuses full access as root.
USER 1000:1000
ENV FERN_POOL=/var/lib/fern \
    DISABLE_AUTOUPDATER=1
ENTRYPOINT ["node", "/opt/t3/apps/server/dist/bin.mjs"]
CMD ["serve"]
