FROM node:22.23.2-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git openssh-client ripgrep jq procps \
    && rm -rf /var/lib/apt/lists/*

ARG OPENCODE_VERSION=0.0.0-next-17444
RUN npm install --global --ignore-scripts=false "@opencode-ai/cli@${OPENCODE_VERSION}" \
    && npm cache clean --force

RUN groupadd --gid 1001 user && useradd --uid 1001 --gid 1001 -m -s /bin/bash user
USER user
WORKDIR /home/user

ENV OPENCODE_DB=/home/user/.local/share/opencode/opencode.db \
    XDG_CONFIG_HOME=/home/user/.local/share/opencode/config
RUN mkdir -p /home/user/workspace /home/user/.local/share/opencode
WORKDIR /home/user/workspace

EXPOSE 4096
CMD ["opencode2", "serve", "--hostname", "0.0.0.0", "--port", "4096"]
