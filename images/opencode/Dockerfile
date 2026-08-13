FROM node:22.23.2-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git openssh-client ripgrep unzip jq procps \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -s /bin/bash user
USER user
WORKDIR /home/user

ARG TARGETARCH
ARG OPENCODE_VERSION=1.18.16
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) archive_arch=x64; checksum=286e07355df06738c1905955be15b7fbc10a7b12d931de9394a6f7597246750b ;; \
      arm64) archive_arch=arm64; checksum=4fdce5f9bc877d977304d71c0c90ad6e83efa381fe0edf0a61e6142a625e1c41 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    archive=/tmp/opencode.tar.gz; \
    curl -fsSL "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${archive_arch}.tar.gz" -o "$archive"; \
    echo "$checksum  $archive" | sha256sum -c -; \
    mkdir -p /home/user/.opencode/bin; \
    tar -xzf "$archive" -C /home/user/.opencode/bin opencode; \
    rm "$archive"
ENV PATH="/home/user/.opencode/bin:${PATH}"

RUN mkdir -p /home/user/workspace /home/user/.local/share/opencode
WORKDIR /home/user/workspace

EXPOSE 4096
CMD ["opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"]
