FROM oven/bun:1.3.14-debian@sha256:9dba1a1b43ce28c9d7931bfc4eb00feb63b0114720a0277a8f939ae4dfc9db6f AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates g++ git make python3 \
    && rm -rf /var/lib/apt/lists/*

ENV OPENCODE_SOURCE_COMMIT=39fb919a054190498f6d5b7985bde231f93ad7a6 \
    OPENCODE_SOURCE_VERSION=0.0.0-source-39fb919a054190498f6d5b7985bde231f93ad7a6

WORKDIR /src
RUN set -eux; \
    test "$(bun --version)" = "1.3.14"; \
    git init .; \
    git remote add origin https://github.com/anomalyco/opencode.git; \
    git fetch --depth=1 origin "${OPENCODE_SOURCE_COMMIT}"; \
    git checkout --detach FETCH_HEAD; \
    test "$(git rev-parse HEAD)" = "${OPENCODE_SOURCE_COMMIT}"; \
    test "$(git rev-parse HEAD^{commit})" = "${OPENCODE_SOURCE_COMMIT}"; \
    test "$(git status --porcelain)" = ""

RUN bun install --frozen-lockfile
RUN OPENCODE_CHANNEL=source OPENCODE_VERSION="${OPENCODE_SOURCE_VERSION}" \
    bun packages/opencode/script/build.ts --single --skip-install \
    && install --mode 0755 \
      "packages/opencode/dist/opencode-linux-$(dpkg --print-architecture | sed 's/amd64/x64/')/bin/opencode" \
      /src/opencode \
    && test "$(/src/opencode --version)" = "${OPENCODE_SOURCE_VERSION}"

FROM node:22.23.2-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

LABEL org.opencontainers.image.source="https://github.com/anomalyco/opencode" \
      org.opencontainers.image.revision="39fb919a054190498f6d5b7985bde231f93ad7a6" \
      org.opencontainers.image.version="0.0.0-source-39fb919a054190498f6d5b7985bde231f93ad7a6" \
      ai.fern.opencode.profile="source-39fb919a054190498f6d5b7985bde231f93ad7a6"

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git openssh-client ripgrep jq procps \
    && rm -rf /var/lib/apt/lists/*

ARG TARGETARCH
ARG GH_VERSION=2.98.0
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) gh_sha256="3b8ac6b30336802fc1a858d7c084e11cdf24ac1a761ca90b68022d7d729208de" ;; \
      arm64) gh_sha256="cf689084f3a3618f7eae4a2420d335d74626d65f5e594b9828d125d69f800d86" ;; \
      *) echo "unsupported GitHub CLI architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    gh_archive="gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    curl --fail --location --silent --show-error \
      --output "/tmp/${gh_archive}" \
      "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${gh_archive}"; \
    echo "${gh_sha256}  /tmp/${gh_archive}" | sha256sum --check --strict; \
    tar --extract --gzip --file "/tmp/${gh_archive}" --directory /tmp; \
    install --mode 0755 "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh" /usr/local/bin/gh; \
    rm --recursive --force "/tmp/${gh_archive}" "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}"; \
    gh --version | grep --quiet "^gh version ${GH_VERSION} "

COPY --from=build /src/opencode /usr/local/bin/opencode
RUN chmod 0755 /usr/local/bin/opencode \
    && test "$(opencode --version)" = "0.0.0-source-39fb919a054190498f6d5b7985bde231f93ad7a6"

RUN groupadd --gid 1001 user && useradd --uid 1001 --gid 1001 -m -s /bin/bash user
USER user
WORKDIR /home/user

ENV XDG_DATA_HOME=/home/user/.local/share \
    XDG_CONFIG_HOME=/home/user/.config
RUN mkdir -p /home/user/workspace /home/user/.local/share/opencode /home/user/.config/opencode /home/user/.config/gh

VOLUME ["/home/user/workspace", "/home/user/.local/share/opencode"]
WORKDIR /home/user/workspace

EXPOSE 4096
CMD ["opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"]
