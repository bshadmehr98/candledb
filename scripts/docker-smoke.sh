#!/usr/bin/env bash
# Builds the container image, starts it, and checks that the server inside
# answers: health, one write, one read, and that the data survives a restart.
# Used by "make docker-smoke" and by the CI workflow.
set -euo pipefail

IMAGE="${IMAGE:-candledb:smoke}"
NAME="candledb-smoke-$$"
VOLUME="candledb-smoke-$$"
PORT="${PORT:-18090}"
KEY="smoke-test-admin-key-0123456789"
URL="http://127.0.0.1:${PORT}"

cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker volume rm "$VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  docker logs "$NAME" >&2 || true
  exit 1
}

wait_healthy() {
  for _ in $(seq 1 50); do
    if curl -fsS "$URL/readyz" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  fail "the server did not become ready"
}

start() {
  docker run -d --name "$NAME" -p "127.0.0.1:${PORT}:8090" \
    -v "$VOLUME:/data" -e CANDLEDB_ADMIN_KEY="$KEY" "$IMAGE" >/dev/null
  wait_healthy
}

echo "== build"
docker build --quiet --build-arg VERSION=smoke -t "$IMAGE" . >/dev/null

echo "== start"
start

echo "== runs as an unprivileged user"
user="$(docker inspect --format '{{.Config.User}}' "$NAME")"
[ -n "$user" ] && [ "$user" != "root" ] && [ "$user" != "0" ] || fail "image user is '$user'"

echo "== write and read with the admin key"
curl -fsS -X PUT "$URL/v1/documents/smoke/one" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"data":{"hello":"world"}}' >/dev/null || fail "write refused"
curl -fsS "$URL/v1/documents/smoke/one" -H "Authorization: Bearer $KEY" \
  | grep -q '"hello":"world"' || fail "read did not return the document"

echo "== rules deny an anonymous read"
code="$(curl -s -o /dev/null -w '%{http_code}' "$URL/v1/documents/smoke/one")"
[ "$code" = 403 ] || [ "$code" = 401 ] || fail "anonymous read returned $code, want 401 or 403"

echo "== data survives a restart"
docker stop -t 20 "$NAME" >/dev/null
[ "$(docker inspect --format '{{.State.ExitCode}}' "$NAME")" = 0 ] || fail "the server did not exit cleanly on SIGTERM"
docker rm "$NAME" >/dev/null
start
curl -fsS "$URL/v1/documents/smoke/one" -H "Authorization: Bearer $KEY" \
  | grep -q '"hello":"world"' || fail "the document was lost in the restart"

echo "ok: container image works"
