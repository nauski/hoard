#!/bin/sh
# Starts the restic REST server (client push target) alongside hoardd (control
# plane). rest-server owns the hot repo directory; hoardd reads/mirrors it.
set -eu

HOT_DIR="${HOARD_HOT_DIR:-/data/hot}"
REST_ADDR="${HOARD_REST_ADDR:-:8000}"
REST_OPTS="${HOARD_REST_OPTS:---no-auth}"   # set to "" and mount .htpasswd for auth

mkdir -p "$HOT_DIR"

# rest-server serves every repo under --path; clients target http://host:8000/hot
echo "[entrypoint] starting rest-server on ${REST_ADDR} (path=/data)"
rest-server --listen "${REST_ADDR}" --path /data ${REST_OPTS} &
REST_PID=$!

term() { echo "[entrypoint] stopping"; kill "$REST_PID" 2>/dev/null || true; }
trap term TERM INT

echo "[entrypoint] starting hoardd"
hoardd "$@" &
HOARD_PID=$!

# Exit if either process dies so the container/orchestrator can restart cleanly.
wait -n "$REST_PID" "$HOARD_PID"
term
exit 1
