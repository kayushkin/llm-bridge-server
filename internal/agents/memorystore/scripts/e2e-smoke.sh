#!/usr/bin/env bash
# Boot-and-answer smoke test for memory-store.
#
# A repo can compile green and still ship a DEAD binary: Go 1.22+
# http.ServeMux panics on conflicting route patterns at *registration* time,
# which no compiler catches. memory-store's RegisterHandlers mounts eight
# overlapping /memories/* patterns (GET /memories/{id} vs GET /memories/recent
# vs POST /memories/search …), so "it builds" proves very little. This script
# builds the standalone binary from THIS checkout, boots it against a throwaway
# DB on a throwaway port, and drives the real HTTP surface — save, read back,
# search, list, delete — asserting on parsed response bodies, not just 200s.
#
# Never touches live state:
#   * temp DB file  (MEMORY_STORE_DB in the temp dir; never ~/.config/memory-store)
#   * temp port     (never the embedded :8160 surface in llm-bridge-server)
#   * HOME is redirected into the temp dir too, so even a bug that ignored
#     MEMORY_STORE_DB and fell back to the default $HOME/.config path could not
#     reach the real memory DB.
#
# No external network: memory-store's embedder is an in-process TF-IDF
# placeholder (embedding.go) and the store is local SQLite, so boot and every
# route are fully hermetic.
#
# Exits 0 on success, non-zero on the first failing assertion; the server log is
# dumped to stderr on failure.
#
# Tunables:
#   E2E_PORT   — listen port (default 19128)
#   E2E_KEEP   — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19128}"
BASE="http://127.0.0.1:$PORT"

for bin in go curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t memory-store-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
FAKE_HOME="$TMP_DIR/home"
DB_PATH="$DATA_DIR/memory.db"
SERVER_LOG="$TMP_DIR/server.log"
RESP="$TMP_DIR/resp.json"
mkdir -p "$BIN_DIR" "$DATA_DIR" "$FAKE_HOME"

SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [ "${E2E_KEEP:-}" = "1" ]; then
    echo "[e2e] keeping $TMP_DIR"
  else
    rm -rf "$TMP_DIR"
  fi
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }
dump_log() {
  echo "----- server.log -----" >&2
  cat "$SERVER_LOG" >&2 2>/dev/null || echo "(no server log)" >&2
  echo "----------------------" >&2
}
fail() {
  echo "FAIL: $*" >&2
  dump_log
  exit 1
}

# api METHOD PATH [JSON_BODY]
#   Writes the response body to $RESP and echoes the HTTP status code. Used for
#   assertions that care about the status (including the 4xx negatives), so
#   deliberately no -f: a non-2xx is data here, not a curl error.
api() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS --max-time 15 -o "$RESP" -w '%{http_code}' \
      -X "$method" "$BASE$path" \
      -H 'Content-Type: application/json' -d "$body"
  else
    curl -sS --max-time 15 -o "$RESP" -w '%{http_code}' -X "$method" "$BASE$path"
  fi
}

# expect_status WANT GOT CONTEXT — fails with the response body attached.
expect_status() {
  local want="$1" got="$2" ctx="$3"
  [ "$got" = "$want" ] || fail "$ctx: expected HTTP $want, got $got — body: $(cat "$RESP")"
}

# jq_eq FILTER WANT CONTEXT — assert that FILTER applied to $RESP yields WANT.
#
# Deliberately NOT written as `[ "$(jq …)" = x ] || fail`: `fail` calls exit,
# and an exit inside a $(…) subshell only kills the subshell — the script would
# print FAIL and then keep running (and still exit 0). Both jq failures and
# mismatches must abort the *parent* shell, so the comparison happens here.
#
# `jq -r`, not `jq -e`: with -e a filter that legitimately yields `false` sets
# exit status 1, indistinguishable from a broken filter.
jq_eq() {
  local filter="$1" want="$2" ctx="$3" got
  got=$(jq -r "$filter" <"$RESP" 2>&1) \
    || fail "$ctx: jq '$filter' errored: $got — body: $(cat "$RESP")"
  [ "$got" = "$want" ] \
    || fail "$ctx: '$filter' expected [$want], got [$got] — body: $(cat "$RESP")"
}

# jq_val FILTER — extract a value from $RESP for the caller to validate itself.
jq_val() { jq -r "$1" <"$RESP"; }

# jq_true FILTER CONTEXT — assert a boolean jq predicate holds. Safe to run
# `fail` from here: this is a statement, not a substitution.
jq_true() {
  jq -e "$1" <"$RESP" >/dev/null 2>&1 \
    || fail "$2 — predicate '$1' did not hold on body: $(cat "$RESP")"
}

step "build memory-store from $REPO_DIR"
cd "$REPO_DIR"
go build -o "$BIN_DIR/memory-store" ./cmd/memory-store
echo "    binary: $(ls -lh "$BIN_DIR/memory-store" | awk '{print $5}')"

step "launch memory-store on :$PORT (db=$DB_PATH)"
# MEMORY_STORE_DB keeps the SQLite file in the temp dir; HOME is redirected as a
# second line of defense (the default db path is $HOME/.config/memory-store).
env HOME="$FAKE_HOME" \
  MEMORY_STORE_ADDR=":$PORT" \
  MEMORY_STORE_DB="$DB_PATH" \
  "$BIN_DIR/memory-store" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

# Poll /health — a route-pattern panic dies during RegisterHandlers, i.e. before
# the listener ever opens, so a boot panic surfaces here as "never became ready"
# plus the panic trace in the dumped log. Abort the instant the pid dies.
READY=0
for _ in $(seq 1 60); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    fail "server process exited during startup (panic on route registration?)"
  fi
  if curl -fsS --max-time 2 "$BASE/health" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 0.2
done
[ "$READY" = "1" ] || fail "server did not answer $BASE/health within ~12s"

STATUS=$(api GET /health)
expect_status 200 "$STATUS" "GET /health"
jq_eq '.status' 'ok' "GET /health"
echo "    health OK"

step "assert the fresh DB landed in the temp dir, not the real one"
[ -f "$DB_PATH" ] || fail "expected a fresh SQLite DB at $DB_PATH"
[ ! -e "$FAKE_HOME/.config/memory-store" ] \
  || fail "server fell back to the default \$HOME db path despite MEMORY_STORE_DB"
echo "    db: $DB_PATH"

step "GET /memories/recent — a virgin DB answers with an empty list"
STATUS=$(api GET /memories/recent)
expect_status 200 "$STATUS" "GET /memories/recent (empty)"
jq_eq 'length' '0' "GET /memories/recent on a fresh DB should be empty"

# ---------------------------------------------------------------------------
# Write path: save a memory through the real route, read it back, search it,
# list it. Responses serialize the Go Memory struct, which carries NO json tags,
# so field keys are the exported Go names (ID, Content, Tags, Importance, …) —
# not lower_snake. The request body, by contrast, uses the lower-snake tags on
# the handler's decode struct (content, tags, importance, is_lazy).
# ---------------------------------------------------------------------------
MEM_ID="e2e-smoke-$$"
MARKER="mnemosyne-marker-$$"

step "POST /memories — save a memory"
STATUS=$(api POST /memories "$(cat <<JSON
{
  "id": "$MEM_ID",
  "content": "boot-and-answer smoke memory $MARKER",
  "summary": "smoke summary",
  "tags": ["e2e", "smoke"],
  "importance": 0.7,
  "source": "system"
}
JSON
)")
expect_status 200 "$STATUS" "POST /memories"
jq_eq '.id' "$MEM_ID" "POST /memories should echo the saved id"
jq_eq '.status' 'saved' "POST /memories should report status=saved"
echo "    saved id: $MEM_ID"

step "GET /memories/$MEM_ID — read back what we wrote"
STATUS=$(api GET "/memories/$MEM_ID")
expect_status 200 "$STATUS" "GET /memories/$MEM_ID"
CTX="GET /memories/$MEM_ID"
jq_eq '.ID' "$MEM_ID" "$CTX"
jq_eq '.Content' "boot-and-answer smoke memory $MARKER" "$CTX (content round-trip)"
jq_eq '.Summary' 'smoke summary' "$CTX (summary round-trip)"
jq_eq '.Source' 'system' "$CTX (source round-trip)"
jq_eq '.Tags | sort | join(",")' 'e2e,smoke' "$CTX (tags round-trip)"
# Importance is nudged up on read (Get bumps it by *1.01), so assert it is at
# least what we stored rather than exact-equal.
jq_true '.Importance >= 0.7' "$CTX: importance should be >= the stored 0.7"
echo "    round-trip OK"

step "POST /memories/search — the saved memory is retrievable by query"
STATUS=$(api POST /memories/search "{\"query\":\"$MARKER\",\"limit\":10}")
expect_status 200 "$STATUS" "POST /memories/search"
jq_true "any(.[]; .ID == \"$MEM_ID\")" "search did not return the memory we just saved"
echo "    search OK"

step "GET /memories/recent — the saved memory shows up in the recent list"
STATUS=$(api GET /memories/recent)
expect_status 200 "$STATUS" "GET /memories/recent (populated)"
jq_true "any(.[]; .ID == \"$MEM_ID\")" "recent list did not contain the saved memory"
echo "    recent OK"

# ---------------------------------------------------------------------------
# Fail-loud negatives — the handler must reject a contentless non-lazy save with
# a 400, and must permit a lazy save with no content.
# ---------------------------------------------------------------------------
step "POST /memories — a contentless non-lazy save fails loudly (400)"
STATUS=$(api POST /memories '{"id":"e2e-empty","content":""}')
expect_status 400 "$STATUS" "POST /memories with empty content"
jq_true '.error | test("content required")' \
  "empty-content save should name the missing content in its error"

step "POST /memories — a lazy reference may be saved without content (200)"
STATUS=$(api POST /memories '{"id":"e2e-lazy","is_lazy":true,"ref_type":"file","ref_target":"/tmp/does-not-matter"}')
expect_status 200 "$STATUS" "POST /memories lazy (no content)"
jq_eq '.status' 'saved' "lazy save should report status=saved"

# ---------------------------------------------------------------------------
# Delete / forget path. DELETE is a documented SOFT delete (management.go
# Forget sets importance to 0; the row survives). So the contract to assert is:
# a forgotten memory drops out of search (searchInternal filters importance > 0)
# but Get still resolves the row, now at importance 0 — NOT a hard row delete.
# ---------------------------------------------------------------------------
step "DELETE /memories/$MEM_ID — forget it (soft delete: importance -> 0)"
STATUS=$(api DELETE "/memories/$MEM_ID")
expect_status 200 "$STATUS" "DELETE /memories/$MEM_ID"
jq_eq '.status' 'deleted' "DELETE should report status=deleted"

step "a forgotten memory drops out of search, but the row survives"
STATUS=$(api POST /memories/search "{\"query\":\"$MARKER\",\"limit\":10}")
expect_status 200 "$STATUS" "POST /memories/search after forget"
jq_true "all(.[]; .ID != \"$MEM_ID\")" \
  "a forgotten memory should no longer be retrievable via search"
STATUS=$(api GET "/memories/$MEM_ID")
expect_status 200 "$STATUS" "GET after forget still resolves the row (soft delete)"
jq_eq '.Importance' '0' "a forgotten memory should read back at importance 0"
echo "    soft-delete contract OK"

step "server is still alive and never logged a panic"
kill -0 "$SERVER_PID" 2>/dev/null || fail "server died during the run"
PANICS=$(grep -c -i -E 'panic:|fatal error:' "$SERVER_LOG" || true)
[ "$PANICS" = "0" ] || fail "server log contains $PANICS panic/fatal line(s)"

step "SUCCESS — memory-store boots, saves, searches, and answers"
echo "    server log: $SERVER_LOG"
