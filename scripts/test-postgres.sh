#!/usr/bin/env bash
# A throwaway Postgres for tests, in a container whose data lives in memory.
#
#   scripts/test-postgres.sh start   start it if it is not running, then print its URL
#   scripts/test-postgres.sh url     print the URL of the running one
#   scripts/test-postgres.sh stop    remove it, and every database in it
#
# Tests that need Postgres read POSTGRESTEST_ADMINISTRATOR_URL:
#
#   export POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh start)"
#   go test ./postgresstorage/...
#
# It listens on 127.0.0.1 only, on a port Docker picks, with a password made at
# start and kept nowhere but the container. Durability is switched off (fsync,
# synchronous_commit, full_page_writes), which is right for tests and wrong for
# anything else: never point a service at it.
set -euo pipefail

CONTAINER_NAME="llm-bridge-test-postgres"
POSTGRES_IMAGE="${POSTGRESTEST_IMAGE:-postgres:17}"

running() {
  [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null)" = "true" ]
}

url() {
  local port password
  port="$(docker port "$CONTAINER_NAME" 5432/tcp | head -1 | sed 's/.*://')"
  password="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$CONTAINER_NAME" | sed -n 's/^POSTGRES_PASSWORD=//p')"
  [ -n "$port" ] && [ -n "$password" ] || { echo "ERROR: could not read the port or password of $CONTAINER_NAME" >&2; exit 1; }
  echo "postgres://postgres:${password}@127.0.0.1:${port}/postgres?sslmode=disable"
}

case "${1:-}" in
  start)
    if ! running; then
      docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
      docker run -d --name "$CONTAINER_NAME" \
        -e POSTGRES_PASSWORD="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
        -p 127.0.0.1::5432 \
        --tmpfs /var/lib/postgresql/data:rw,size=2g \
        --shm-size 256m \
        "$POSTGRES_IMAGE" \
        -c fsync=off -c synchronous_commit=off -c full_page_writes=off -c max_connections=300 >/dev/null
      for _ in $(seq 1 60); do
        # The image starts a temporary server to initialize, then the real one;
        # only the real one answers over TCP.
        if docker exec "$CONTAINER_NAME" pg_isready -q -h 127.0.0.1 -U postgres 2>/dev/null; then break; fi
        sleep 0.5
      done
      docker exec "$CONTAINER_NAME" pg_isready -q -h 127.0.0.1 -U postgres || {
        echo "ERROR: $CONTAINER_NAME did not become ready; see: docker logs $CONTAINER_NAME" >&2
        exit 1
      }
    fi
    url
    ;;
  url)
    running || { echo "ERROR: $CONTAINER_NAME is not running; run: $0 start" >&2; exit 1; }
    url
    ;;
  stop)
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
    ;;
  *)
    echo "usage: $0 start|url|stop" >&2
    exit 2
    ;;
esac
