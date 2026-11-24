#!/usr/bin/env bash
# Boot-and-answer smoke test for noteboard.
#
# Builds the server from THIS checkout, boots it against a throwaway SQLite
# database on a throwaway port, and drives a real create → read → search →
# rerank → update → delete → restore lifecycle over HTTP. The live service
# (:8191, ~/.noteboard/noteboard.db) is never touched.
#
# It covers /health, /api/items (GET, POST), /api/items/{id} (GET, PATCH,
# DELETE), /api/items/{id}/restore, /api/items/{id}/revisions,
# /api/items/rerank, /api/tags and /api/search. It does NOT yet cover
# /api/items/{id}/hold, /unhold, /occurrences or /api/lists.
#
# Why this exists: `go build` passing proves nothing about whether the binary
# can BOOT. Two failure classes here are invisible to the compiler:
#
#   1. FTS5. internal/db.migrate() runs `CREATE VIRTUAL TABLE ... USING fts5`,
#      which only resolves if the SQLite driver ships the FTS5 module. The
#      /api/search assertion below is what actually exercises it at runtime.
#      This used to be a live trap: under mattn/go-sqlite3, FTS5 arrived only
#      via CGO_CFLAGS=-DSQLITE_ENABLE_FTS5, which only the Makefile passed, so a
#      plain `go build` produced a binary that compiled green and then died at
#      boot with "no such module: fts5". The driver is now modernc.org/sqlite
#      (pure Go, FTS5 built in) and the build below sets CGO_ENABLED=0 — so if
#      a cgo SQLite driver ever comes back, this smoke fails at the build step
#      rather than shipping a binary that cannot open its own database.
#   2. Route registration. internal/api.Handler() registers overlapping
#      patterns ("/api/items/rerank" inside "/api/items/"). http.ServeMux
#      panics on a conflicting pattern at REGISTRATION time — no compiler sees
#      it, and a smoke that only curls /health would not either. The rerank
#      assertion below fails loudly if that route stops resolving.
#
# Exits 0 on success, non-zero on the first failing assertion. On failure the
# server log is dumped to stderr.
#
# Tunables:
#   E2E_PORT  — listen port (default 19101; NOT the live 8191)
#   E2E_KEEP  — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19101}"
BASE="http://127.0.0.1:$PORT"

for bin in go curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done
TMP_DIR="$(mktemp -d -t noteboard-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
DB_PATH="$DATA_DIR/noteboard.db"
LOG="$TMP_DIR/server.log"
mkdir -p "$BIN_DIR" "$DATA_DIR"

SERVER_PID=""
DUMPED=0

dump_log() {
  [ "$DUMPED" = "1" ] && return 0
  DUMPED=1
  if [ -s "$LOG" ]; then
    echo "----- server.log -----" >&2
    cat "$LOG" >&2
    echo "----------------------" >&2
  fi
}

cleanup() {
  # Capture the exit status FIRST — anything below would clobber it. Dumping the
  # log here (not just in fail()) is what covers the abort paths set -e takes on
  # its own, e.g. a `curl -fsS` that returns non-2xx inside a $(...) assignment.
  local status=$?
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [ "$status" -ne 0 ]; then
    dump_log
  fi
  if [ "${E2E_KEEP:-}" = "1" ]; then
    echo "[e2e] keeping $TMP_DIR"
  else
    rm -rf "$TMP_DIR"
  fi
  return "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }
fail() {
  echo "FAIL: $*" >&2
  dump_log
  exit 1
}

# Refuse to run if something already owns the port — otherwise every assertion
# below would silently be testing THAT process (e.g. the live service).
if curl -fsS -o /dev/null --max-time 2 "$BASE/health" 2>/dev/null; then
  echo "ERROR: something is already listening on $BASE — set E2E_PORT" >&2
  exit 2
fi

step "build noteboard from $REPO_DIR"
cd "$REPO_DIR"
# Default flags, cgo off, mirroring ./Makefile — no special environment. That is
# the point: the build a person or a build guard gets by typing `go build` has
# to be the build that boots. See the FTS5 note in this file's header.
CGO_ENABLED=0 go build -o "$BIN_DIR/noteboard" ./cmd/noteboard/
echo "    binary: $BIN_DIR/noteboard ($(ls -lh "$BIN_DIR/noteboard" | awk '{print $5}'))"

step "launch noteboard on :$PORT (db: $DB_PATH)"
# Both knobs are env-only (cmd/noteboard/main.go). Pointing them at the temp
# dir is what keeps the live DB and the live port out of this run.
NOTEBOARD_PORT="$PORT" \
NOTEBOARD_DB="$DB_PATH" \
  "$BIN_DIR/noteboard" >"$LOG" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

# Poll for readiness — never sleep-and-hope. The server has to open SQLite, run
# migrations and build the FTS index before it binds, and that is not a fixed
# cost. Give it ~15s, but bail immediately if the process is already dead.
READY=0
for _ in $(seq 1 60); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    fail "server exited during startup (see log below)"
  fi
  if curl -fsS -o /dev/null --max-time 2 "$BASE/health" 2>/dev/null; then
    READY=1
    break
  fi
  sleep 0.25
done
[ "$READY" = "1" ] || fail "server did not answer on $BASE/health within ~15s"

step "GET /health"
HEALTH="$(curl -fsS "$BASE/health")"
echo "    $HEALTH"
[ "$(jq -r '.status' <<<"$HEALTH")" = "ok" ] || fail "/health status not ok: $HEALTH"
# A fresh DB has zero items. If this is ever non-zero we are talking to a
# populated database — i.e. the live one — and every assertion below is a lie.
COUNT0="$(jq -r '.items' <<<"$HEALTH")"
[ "$COUNT0" = "0" ] \
  || fail "expected an empty temp DB but /health reports $COUNT0 items — is this the LIVE db?"

# Unique token so the FTS5 search assertion cannot match anything but our item.
TOKEN="e2esmoke$$$(date +%s)"
TAG="e2e-smoke-tag"

step "POST /api/items (todo)"
CREATED="$(curl -fsS -X POST "$BASE/api/items" \
  -H 'Content-Type: application/json' \
  -d "{\"type\":\"todo\",\"title\":\"$TOKEN smoke item\",\"body\":\"body of $TOKEN\",\"tags\":[\"$TAG\"],\"priority\":2}")"
ID="$(jq -r '.id' <<<"$CREATED")"
[ -n "$ID" ] && [ "$ID" != "null" ] || fail "POST /api/items returned no id: $CREATED"
echo "    id: $ID"
[ "$(jq -r '.type'     <<<"$CREATED")" = "todo" ]   || fail "created item type != todo: $CREATED"
[ "$(jq -r '.status'   <<<"$CREATED")" = "open" ]   || fail "created item status != open: $CREATED"
[ "$(jq -r '.priority' <<<"$CREATED")" = "2" ]      || fail "created item priority != 2: $CREATED"

step "GET /api/items/$ID — read back what we wrote"
GOT="$(curl -fsS "$BASE/api/items/$ID")"
[ "$(jq -r '.title'    <<<"$GOT")" = "$TOKEN smoke item" ] || fail "title did not round-trip: $GOT"
[ "$(jq -r '.body'     <<<"$GOT")" = "body of $TOKEN" ]    || fail "body did not round-trip: $GOT"
[ "$(jq -r '.tags[0]'  <<<"$GOT")" = "$TAG" ]              || fail "tag did not round-trip: $GOT"
echo "    title/body/tags round-tripped"

step "GET /api/items?type=todo&tag=$TAG — list filter finds it"
LISTED="$(curl -fsS "$BASE/api/items?type=todo&tag=$TAG&status=open")"
[ "$(jq -r --arg id "$ID" '[.[] | select(.id==$id)] | length' <<<"$LISTED")" = "1" ] \
  || fail "filtered list did not contain $ID: $LISTED"
echo "    found in filtered list"

step "GET /api/search?q=$TOKEN — FTS5 index is live"
# This is the assertion that proves the fts5 module is actually compiled in:
# a non-FTS5 build cannot even reach here (it dies in migrate at boot), and a
# broken index returns nothing.
FOUND="$(curl -fsS "$BASE/api/search?q=$TOKEN")"
[ "$(jq -r --arg id "$ID" '[.[] | select(.id==$id)] | length' <<<"$FOUND")" = "1" ] \
  || fail "FTS5 search for '$TOKEN' did not return $ID: $FOUND"
echo "    search hit"

step "POST /api/items/rerank — the route nested under /api/items/ still resolves"
curl -fsS -X POST "$BASE/api/items/rerank" \
  -H 'Content-Type: application/json' \
  -d "{\"items\":[{\"id\":\"$ID\",\"rank\":3.5}]}" >/dev/null \
  || fail "POST /api/items/rerank did not answer 2xx — route missing, or shadowed by /api/items/ (which would treat 'rerank' as an item id)"
RANK="$(curl -fsS "$BASE/api/items/$ID" | jq -r '.rank')"
[ "$RANK" = "3.5" ] || fail "rerank did not stick (rank=$RANK, want 3.5) — is /api/items/rerank shadowed by /api/items/?"
echo "    rank: $RANK"

step "GET /api/tags — tag aggregation sees our tag"
TAGS="$(curl -fsS "$BASE/api/tags")"
[ "$(jq -r --arg t "$TAG" '[.[] | select(.tag==$t)] | length' <<<"$TAGS")" = "1" ] \
  || fail "/api/tags did not include $TAG: $TAGS"
echo "    tag present"

step "PATCH /api/items/$ID {status:done}"
PATCHED="$(curl -fsS -X PATCH "$BASE/api/items/$ID" \
  -H 'Content-Type: application/json' -d '{"status":"done"}')"
[ "$(jq -r '.status' <<<"$PATCHED")" = "done" ] || fail "PATCH did not set status=done: $PATCHED"
# The write must be durable, not just echoed back by the handler.
[ "$(curl -fsS "$BASE/api/items/$ID" | jq -r '.status')" = "done" ] \
  || fail "status=done did not persist to the DB"
echo "    status: done (persisted)"

step "DELETE /api/items/$ID — reversible delete, and it does not touch status"
curl -fsS -X DELETE "$BASE/api/items/$ID" >/dev/null \
  || fail "DELETE /api/items/$ID did not answer 2xx"
# A deleted item is gone from every read path. Asking for it by id 404s; only a
# caller that explicitly wants the tombstone gets one.
curl -fsS -o /dev/null "$BASE/api/items/$ID" 2>/dev/null \
  && fail "GET returned a deleted item — a soft delete any read still returns is not a delete"
[ "$(curl -fsS "$BASE/api/items/$ID?include_deleted=true" | jq -r '.deleted_at')" != "null" ] \
  || fail "DELETE did not stamp deleted_at"
# The status the item had is preserved, NOT overwritten with 'archived' — that
# overload is the bug this replaced, and it made deletes indistinguishable from
# a user archiving something on purpose.
[ "$(curl -fsS "$BASE/api/items/$ID?include_deleted=true" | jq -r '.status')" = "done" ] \
  || fail "DELETE clobbered status; it must leave the item's own state alone"
COUNT1="$(curl -fsS "$BASE/health" | jq -r '.items')"
[ "$COUNT1" = "0" ] || fail "expected 0 live items after delete, got $COUNT1"
echo "    deleted (status preserved); live item count back to 0"

step "POST /api/items/$ID/restore — the delete is undoable"
curl -fsS -X POST "$BASE/api/items/$ID/restore" >/dev/null \
  || fail "restore did not answer 2xx"
[ "$(curl -fsS "$BASE/api/items/$ID" | jq -r '.status')" = "done" ] \
  || fail "restored item did not come back in the state it was deleted in"
echo "    restored intact"

step "GET /api/items/$ID/revisions — every mutation left a trail"
REVS="$(curl -fsS "$BASE/api/items/$ID/revisions" | jq -r 'length')"
[ "$REVS" -ge 3 ] \
  || fail "expected update+delete+restore to be recorded, got $REVS revisions"
curl -fsS "$BASE/api/items/$ID/revisions" | jq -e 'map(.reason) | index("delete")' >/dev/null \
  || fail "the delete was not recorded in the change log"
echo "    $REVS revisions recorded; prior states recoverable"

# Put the board back to empty so the hermetic check below sees what it expects.
curl -fsS -X DELETE "$BASE/api/items/$ID" >/dev/null \
  || fail "final DELETE did not answer 2xx"

step "confirm the run was hermetic"
[ -f "$DB_PATH" ] || fail "temp DB $DB_PATH was never created — where did the data go?"
echo "    wrote only $DB_PATH"

step "SUCCESS — noteboard boots and answers"
echo "    server log: $LOG"
