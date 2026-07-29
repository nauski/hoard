# Build hoardd and bundle it with restic + rest-server in one small image.
# Multi-stage: compile Go, then assemble a minimal Debian runtime that also
# ships the restic REST server so clients have a push target out of the box.

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod ./
# No external modules yet; if that changes, add: COPY go.sum ./ && go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hoardd ./cmd/hoardd

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends restic rest-server ca-certificates tini \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/hoardd /usr/local/bin/hoardd
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# 8080 = hoard dashboard/API, 8000 = restic rest-server (client push target)
EXPOSE 8080 8000
VOLUME ["/data"]

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
