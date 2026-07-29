# Build hoardd and bundle it with pinned restic + rest-server binaries in one
# small image. We fetch the release binaries directly (rather than apt) so the
# versions are reproducible and current: Debian's restic lags several releases,
# and rest-server isn't packaged at all.

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod ./
# No external modules yet; if that changes, add: COPY go.sum ./ && go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hoardd ./cmd/hoardd

# Fetch pinned tool binaries in a stage with curl available.
FROM debian:bookworm-slim AS tools
ARG RESTIC_VERSION=0.18.0
ARG RESTSERVER_VERSION=0.14.0
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates bzip2 \
 && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    curl -fsSL -o /tmp/restic.bz2 \
      "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_amd64.bz2"; \
    bunzip2 /tmp/restic.bz2; \
    install -m 0755 /tmp/restic /usr/local/bin/restic; \
    curl -fsSL -o /tmp/rest-server.tgz \
      "https://github.com/restic/rest-server/releases/download/v${RESTSERVER_VERSION}/rest-server_${RESTSERVER_VERSION}_linux_amd64.tar.gz"; \
    tar -xzf /tmp/rest-server.tgz -C /tmp; \
    install -m 0755 /tmp/rest-server_${RESTSERVER_VERSION}_linux_amd64/rest-server /usr/local/bin/rest-server; \
    restic version; rest-server --version || true

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tini \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/hoardd /usr/local/bin/hoardd
COPY --from=tools /usr/local/bin/restic /usr/local/bin/restic
COPY --from=tools /usr/local/bin/rest-server /usr/local/bin/rest-server
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# 8080 = hoard dashboard/API, 8000 = restic rest-server (client push target)
EXPOSE 8080 8000
VOLUME ["/data"]

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
