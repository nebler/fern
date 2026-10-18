# syntax=docker/dockerfile:1
# A box: Debian slim with Postgres, and fern-init as PID 1.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/fern-init ./cmd/fern-init

FROM debian:trixie-slim
# Postgres binaries only: clusters live in each environment's live/.
RUN mkdir -p /etc/postgresql-common/createcluster.d \
 && echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.d/fern.conf \
 && apt-get update \
 && apt-get install -y --no-install-recommends postgresql-17 ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 1000 --user-group --home-dir /tmp --no-create-home fern
COPY --from=build /out/fern-init /usr/local/bin/fern-init
ENV PATH=/usr/lib/postgresql/17/bin:$PATH \
    HOME=/tmp PGHOST=/tmp PGUSER=postgres PGDATABASE=postgres
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/fern-init"]
