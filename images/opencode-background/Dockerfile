FROM node:22.23.2-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

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

ARG OPENCODE_VERSION=1.18.16
RUN npm install --global --ignore-scripts=false "opencode-ai@${OPENCODE_VERSION}" \
    && test "$(opencode --version)" = "${OPENCODE_VERSION}" \
    && npm cache clean --force

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
