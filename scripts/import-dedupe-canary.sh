#!/usr/bin/env bash
# Cross-service check for the discovery import dedupe guard.
#
# Boots a REAL log-store from the sibling checkout on a throwaway port and
# database, then runs the guard's Go test against it. Everything else in
# internal/harness talks to logStoreStub, a hand-written mirror of log-store's
# HTTP surface that lives in THIS repo — and a mirror agrees with itself
# forever. If log-store renames the field, moves the route, or changes what an
# unknown id returns, the stub keeps answering the old way, the unit tests stay
# green, and the live gateway goes back to re-importing every transcript on
# disk. This script is the only thing that would notice.
#
# What the guard is for: discovery used to ask THIS process's database whether
# a session was new and write the answer's consequence into log-store. A
# gateway booted with a fresh database and the default log-store URL therefore
# re-imported everything; on 2026-08-01 a canary with an isolated
# LLMBRIDGE_DB_PATH but no LLMBRIDGE_LOG_STORE_URL pushed 2,863 duplicate
# sessions into the production store in two minutes.
#
# Hermetic by construction: temp dir, temp DB, temp port. The production
# log-store on :8175 and its DB at ~/.config/log-store/events.db are never
# touched, and the Go test refuses a URL naming :8175 outright.
#
# Exits 0 on success, non-zero on the first failure, dumping log-store's log.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_STORE_SRC="${LOG_STORE_SRC:-$REPO_ROOT/../log-store}"

[ -d "$LOG_STORE_SRC" ] || { echo "FAIL: no log-store checkout at $LOG_STORE_SRC" >&2; exit 1; }

WORK="$(mktemp -d)"
SERVER_LOG="$WORK/log-store.log"
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# A free port from the kernel rather than a guess, so two concurrent runs of
# this script cannot collide.
PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
BASE="http://127.0.0.1:$PORT"

echo "==> build log-store from $LOG_STORE_SRC"
(cd "$LOG_STORE_SRC" && go build -o "$WORK/log-store" ./cmd/log-store)

echo "==> boot log-store on :$PORT (db: $WORK/events.db)"
LOG_STORE_LISTEN_ADDR=":$PORT" \
LOG_STORE_DB_PATH="$WORK/events.db" \
LOG_STORE_LOGSTACK_URL="http://127.0.0.1:1" \
  "$WORK/log-store" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

# Poll for readiness. A sleep long enough to be safe is a sleep that wastes
# time on every run and still races on a loaded box.
for _ in $(seq 1 100); do
  if curl -fsS --max-time 2 "$BASE/health" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "FAIL: log-store exited during boot" >&2; cat "$SERVER_LOG" >&2; exit 1
  fi
  sleep 0.1
done
curl -fsS --max-time 2 "$BASE/health" >/dev/null || {
  echo "FAIL: log-store never became ready on $BASE" >&2; cat "$SERVER_LOG" >&2; exit 1
}

echo "==> run the guard against it"
cd "$REPO_ROOT"
if ! LLMBRIDGE_CANARY_LOG_STORE_URL="$BASE" \
     go test ./internal/harness/ -count=1 -v -run TestImportHistoryAgainstRealLogStore; then
  echo "--- log-store log ---" >&2; cat "$SERVER_LOG" >&2
  exit 1
fi

echo "==> SUCCESS — the dedupe guard agrees with a real log-store"
