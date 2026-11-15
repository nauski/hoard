#!/bin/sh
# Starts the restic REST server (client push target) alongside hoardd (control
# plane). rest-server owns the hot repo directory; hoardd reads/mirrors it.
set -eu

HOT_DIR="${HOARD_HOT_DIR:-/data/hot}"
REST_ADDR="${HOARD_REST_ADDR:-:8000}"
REST_OPTS="${HOARD_REST_OPTS:---no-auth}"   # set to "" and mount .htpasswd for auth
REST_PATH="$(dirname "$HOT_DIR")"
# Private repositories: every enrolled client gets its own repository under
# <data>/repos/<name> and its own login, and can reach nothing else. hoardd
# reads the same variable, creates the logins on enrollment and keeps the login
# file up to date (rest-server reloads it by itself). Clients enrolled before
# this was turned on must enroll again.
if [ "${HOARD_REST_PRIVATE:-false}" = "true" ]; then
  REST_PATH="${REST_PATH}/repos"
  mkdir -p "$REST_PATH"
  HTPASSWD="$(dirname "$HOT_DIR")/htpasswd"
  [ -f "$HTPASSWD" ] || : > "$HTPASSWD"
  chmod 600 "$HTPASSWD"
  REST_OPTS="--private-repos --htpasswd-file ${HTPASSWD}"
fi
# Append-only: clients can add backups but cannot delete or overwrite them, so a
# compromised or misbehaving client cannot destroy its own history. Deletes,
# retention and unpin/release still work from the hoard dashboard, because
# hoardd writes to the hot repo on disk, not through rest-server.
if [ "${HOARD_REST_APPEND_ONLY:-false}" = "true" ]; then
  REST_OPTS="${REST_OPTS} --append-only"
fi

mkdir -p "$HOT_DIR"

# rest-server serves every repo under --path; clients target http://host:8000/hot
echo "[entrypoint] starting rest-server on ${REST_ADDR} (path=${REST_PATH})"
rest-server --listen "${REST_ADDR}" --path "${REST_PATH}" ${REST_OPTS} &
REST_PID=$!

term() { echo "[entrypoint] stopping"; kill "$REST_PID" 2>/dev/null || true; }
trap term TERM INT

echo "[entrypoint] starting hoardd"
hoardd "$@" &
HOARD_PID=$!

# Exit if either process dies so the container/orchestrator can restart cleanly.
# POSIX sh (dash) has no `wait -n`, so poll both PIDs instead.
while kill -0 "$REST_PID" 2>/dev/null && kill -0 "$HOARD_PID" 2>/dev/null; do
  sleep 5
done
echo "[entrypoint] a supervised process exited; shutting down"
term
exit 1
