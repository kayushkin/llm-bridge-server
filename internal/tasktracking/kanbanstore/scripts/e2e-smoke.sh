#!/usr/bin/env bash
# Boot-and-answer smoke test for kanban-store.
#
# Builds cmd/kanban-store from THIS checkout, boots it against a throwaway
# SQLite file on a throwaway port, and drives real HTTP routes. Proves the
# committed tree produces a binary that BOOTS (a Go 1.22+ ServeMux route
# conflict panics at registration time — it compiles green and dies on
# start) and ANSWERS (writes and reads back real board/column/card state).
#
# Never touches live state: the live service runs on :8305 against
# ~/.kanban-store/kanban-store.db. This script uses neither.
#
# Dependency handling — kanban-store's cards ARE noteboard items:
#   Phase A boots with KANBAN_NOTEBOARD_URL pointed at an unreachable
#     address, proving the binary starts with noteboard down and that the
#     noteboard-backed routes degrade to 502 instead of panicking. All
#     noteboard-independent routes (boards, columns, links, entity tags,
#     WIP enforcement) are asserted for real here.
#   Phase B restarts against an in-script stub noteboard (python3, in-memory,
#     the endpoints internal/noteboard/client.go actually calls) so
#     the card lifecycle — create, board-view assembly, move + auto_status
#     write-back, attach/detach — is exercised end to end. The real noteboard
#     on :8191 is never contacted.
#
# Exits 0 on success, non-zero on the first failing assertion; dumps the
# server log to stderr on failure.
#
# Tunables:
#   E2E_PORT            — kanban-store listen port (default 19107)
#   E2E_NOTEBOARD_PORT  — stub noteboard listen port (default 19106)
#   E2E_KEEP            — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
# Every route but /health is gated, so this run has its own throwaway token: the
# smoke acts as an internal service, which is the shape every assertion below
# was written against. The per-principal rules are unit-tested, not smoked here.
SERVICE_TOKEN="e2e-smoke-service-token-000000000000000000"
PORT="${E2E_PORT:-19107}"
NB_PORT="${E2E_NOTEBOARD_PORT:-19106}"
BASE="http://127.0.0.1:$PORT"
NB_BASE="http://127.0.0.1:$NB_PORT"
# Port 1 (tcpmux) is not listening: connect() gets ECONNREFUSED immediately,
# so noteboard calls fail fast rather than hanging on the client's 10s timeout.
NB_UNREACHABLE="http://127.0.0.1:1"

# go-sqlite3 is cgo, so a C compiler is mandatory — without it `go build`
# fails with a confusing linker error under the nightly job's minimal env.
for bin in go curl jq python3 cc; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t kanban-store-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
DB_PATH="$DATA_DIR/kanban-store.db"
BODY="$TMP_DIR/body.json"
mkdir -p "$BIN_DIR" "$DATA_DIR"

SERVER_PID=""
NB_PID=""
cleanup() {
  for pid in "$SERVER_PID" "$NB_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [ "${E2E_KEEP:-}" = "1" ]; then
    echo "[e2e] keeping $TMP_DIR"
  else
    rm -rf "$TMP_DIR"
  fi
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }
fail() {
  echo "FAIL: $*" >&2
  if [ -f "$TMP_DIR/server.log" ]; then
    echo "----- server.log -----" >&2
    cat "$TMP_DIR/server.log" >&2
  fi
  if [ -f "$TMP_DIR/noteboard-stub.log" ]; then
    echo "----- noteboard-stub.log -----" >&2
    cat "$TMP_DIR/noteboard-stub.log" >&2
  fi
  exit 1
}

# req METHOD PATH [JSON] — prints the HTTP status code; response body lands in $BODY.
# No -f: 4xx/5xx are expected outcomes for some assertions and must not abort.
# A connection-level failure still exits non-zero (set -e), which is what we want.
req() {
  local method="$1" path="$2" data="${3:-}"
  local args=(-sS -o "$BODY" -w '%{http_code}' --max-time 15 -X "$method"
    -H "X-Kanban-Store-Service-Token: $SERVICE_TOKEN" "$BASE$path")
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' -d "$data")
  fi
  curl "${args[@]}"
}

# expect CODE GOT WHAT — assert an HTTP status, dumping the body on mismatch.
expect() {
  local want="$1" got="$2" what="$3"
  if [ "$want" != "$got" ]; then
    echo "----- response body -----" >&2
    cat "$BODY" >&2
    echo >&2
    fail "$what: expected HTTP $want, got $got"
  fi
}

# jget FILTER — read a jq value out of the last response body.
jget() { jq -r "$1" "$BODY"; }

# assert_eq WANT GOT WHAT
assert_eq() {
  [ "$1" = "$2" ] || fail "$3: expected '$1', got '$2'"
}

start_server() {
  local noteboard_url="$1" label="$2"
  step "boot kanban-store on :$PORT ($label)"
  # principal-store is never contacted: the only route that calls it (card
  # assignment) is asserted on its fail-loud path below, against a closed port.
  KANBAN_PORT="$PORT" \
  KANBAN_DB="$DB_PATH" \
  KANBAN_NOTEBOARD_URL="$noteboard_url" \
  PRINCIPAL_STORE_URL="$NB_UNREACHABLE" \
  KANBAN_STORE_SERVICE_TOKEN="$SERVICE_TOKEN" \
  GRANT_STORE_URL="$NB_UNREACHABLE" \
  GRANT_STORE_SERVICE_TOKEN="$SERVICE_TOKEN" \
    "$BIN_DIR/kanban-store" >>"$TMP_DIR/server.log" 2>&1 &
  SERVER_PID=$!
  echo "    pid: $SERVER_PID  db: $DB_PATH  noteboard: $noteboard_url"

  # Poll — never sleep-and-hope. ~15s budget.
  local ok=""
  for _ in $(seq 1 75); do
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
      fail "kanban-store exited during startup (route-registration panic? db migration?)"
    fi
    if curl -fsS --max-time 2 -o /dev/null "$BASE/health" 2>/dev/null; then ok=1; break; fi
    sleep 0.2
  done
  [ -n "$ok" ] || fail "kanban-store did not answer /health on $BASE within ~15s"
  echo "    /health OK"
}

stop_server() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  SERVER_PID=""
}

# ============================================================================
step "build cmd/kanban-store from $REPO_DIR"
cd "$REPO_DIR"
CGO_ENABLED=1 go build -o "$BIN_DIR/kanban-store" ./cmd/kanban-store
echo "    binary: $(ls -lh "$BIN_DIR/kanban-store" | awk '{print $5}')"

# ============================================================================
# PHASE A — noteboard unreachable.
# ============================================================================
start_server "$NB_UNREACHABLE" "noteboard UNREACHABLE"

step "GET /health — fresh db, all counts zero"
CODE=$(req GET /health); expect 200 "$CODE" "/health"
assert_eq ok      "$(jget '.status')"          "health status"
assert_eq 0       "$(jget '.counts.boards')"   "fresh db board count"
assert_eq 0       "$(jget '.counts.columns')"  "fresh db column count"
assert_eq 0       "$(jget '.counts.placements')" "fresh db placement count"

step "GET /settings — the registry the server reads from is the one it serves"
# A registry that is built and never read from would show the default port, and
# no unit test sees that: only a real boot on a port of the smoke's choosing does.
CODE=$(req GET /settings); expect 200 "$CODE" "/settings with the service token"
assert_eq kanban-store "$(jget '.service')" "settings service name"
assert_eq "$PORT"      "$(jget '.settings[] | select(.key=="listen_port") | .value')"  "settings listen_port is the port this smoke chose"
assert_eq environment  "$(jget '.settings[] | select(.key=="listen_port") | .source')" "settings listen_port source"
assert_eq "$DB_PATH"   "$(jget '.settings[] | select(.key=="database_path") | .value')" "settings database_path"
if grep -qF "$SERVICE_TOKEN" "$BODY"; then fail "GET /settings carries the service token"; fi
assert_eq 401 "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 "$BASE/settings")" "/settings with no caller"
CODE=$(req PUT /settings/noteboard_url '{"value":"http://elsewhere.example"}'); expect 404 "$CODE" "PUT /settings/{key} has no route"

step "POST /api/boards — create a board and read it back"
BOARD_NAME="e2e smoke board $$"
CODE=$(req POST /api/boards "{\"name\":\"$BOARD_NAME\",\"description\":\"created by e2e-smoke\"}")
expect 201 "$CODE" "POST /api/boards"
BOARD_ID="$(jget '.id')"
[ -n "$BOARD_ID" ] && [ "$BOARD_ID" != "null" ] || fail "POST /api/boards returned no id"
assert_eq "$BOARD_NAME" "$(jget '.name')" "created board name"
echo "    board id: $BOARD_ID"

CODE=$(req GET "/api/boards/$BOARD_ID"); expect 200 "$CODE" "GET /api/boards/:id"
assert_eq "$BOARD_NAME"        "$(jget '.name')"        "read-back board name"
assert_eq "created by e2e-smoke" "$(jget '.description')" "read-back board description"
assert_eq false                "$(jget '.archived')"    "read-back board archived"

step "POST /api/boards — reject a board with no name"
CODE=$(req POST /api/boards '{"description":"nameless"}')
expect 400 "$CODE" "POST /api/boards (no name)"

step "GET /api/boards — the new board is in the list"
CODE=$(req GET /api/boards); expect 200 "$CODE" "GET /api/boards"
assert_eq "$BOARD_NAME" "$(jget ".[] | select(.id==\"$BOARD_ID\") | .name")" "board present in list"

step "POST /api/boards/:id/columns — Todo, Doing, Done(auto_status), Blocked(wip=0)"
CODE=$(req POST "/api/boards/$BOARD_ID/columns" '{"name":"Todo","position":1}')
expect 201 "$CODE" "POST column Todo"
TODO_COL="$(jget '.id')"; assert_eq Todo "$(jget '.name')" "column Todo name"

CODE=$(req POST "/api/boards/$BOARD_ID/columns" '{"name":"Doing","position":2,"wip_limit":5,"color":"#4488ff"}')
expect 201 "$CODE" "POST column Doing"
DOING_COL="$(jget '.id')"; assert_eq 5 "$(jget '.wip_limit')" "column Doing wip_limit"

CODE=$(req POST "/api/boards/$BOARD_ID/columns" '{"name":"Done","position":3,"auto_status":"done","work_state":"done"}')
expect 201 "$CODE" "POST column Done"
DONE_COL="$(jget '.id')"; assert_eq done "$(jget '.auto_status')" "column Done auto_status"
assert_eq done "$(jget '.work_state')" "column Done work_state"

CODE=$(req POST "/api/boards/$BOARD_ID/columns" '{"name":"Blocked","position":4,"wip_limit":0}')
expect 201 "$CODE" "POST column Blocked"
BLOCKED_COL="$(jget '.id')"
echo "    todo=$TODO_COL doing=$DOING_COL done=$DONE_COL blocked=$BLOCKED_COL"

step "GET /api/work-states — the shared vocabulary; a column outside it is a 400"
CODE=$(req GET /api/work-states); expect 200 "$CODE" "GET work-states"
assert_eq "not_started working waiting done dropped" "$(jq -r '[.[].work_state] | join(" ")' "$BODY")" "work-state vocabulary"
CODE=$(req POST "/api/boards/$BOARD_ID/columns" '{"name":"Bogus","work_state":"blocked"}')
expect 400 "$CODE" "POST column with work_state outside the vocabulary"

step "POST /api/boards/:id/columns — reject an invalid auto_status"
CODE=$(req POST "/api/boards/$BOARD_ID/columns" '{"name":"Bogus","auto_status":"finished"}')
expect 400 "$CODE" "POST column with auto_status=finished"

step "GET /api/boards/:id/columns — read back in position order"
CODE=$(req GET "/api/boards/$BOARD_ID/columns"); expect 200 "$CODE" "GET columns"
assert_eq 4 "$(jget 'length')" "column count"
assert_eq "Todo Doing Done Blocked" "$(jget '[.[].name] | join(" ")')" "columns in position order"

step "PATCH /api/columns/:id — rename Doing → In Progress, lift wip_limit"
CODE=$(req PATCH "/api/columns/$DOING_COL" '{"name":"In Progress","wip_limit":3}')
expect 200 "$CODE" "PATCH column"
assert_eq "In Progress" "$(jget '.name')"      "patched column name"
assert_eq 3             "$(jget '.wip_limit')" "patched column wip_limit"
CODE=$(req GET "/api/columns/$DOING_COL"); expect 200 "$CODE" "GET column"
assert_eq "In Progress" "$(jget '.name')" "re-read patched column name"

step "PATCH /api/columns/:id — reject an invalid auto_status"
CODE=$(req PATCH "/api/columns/$DOING_COL" '{"auto_status":"nope"}')
expect 400 "$CODE" "PATCH column with auto_status=nope"

step "POST /api/boards/:id/columns/reorder — swap Todo and In Progress"
CODE=$(req POST "/api/boards/$BOARD_ID/columns/reorder" \
  "{\"columns\":[{\"id\":\"$DOING_COL\",\"position\":0.5}]}")
expect 200 "$CODE" "POST columns/reorder"
assert_eq "In Progress" "$(jget '.[0].name')" "reorder put In Progress first"
# put it back so later phases read a sane board
CODE=$(req POST "/api/boards/$BOARD_ID/columns/reorder" \
  "{\"columns\":[{\"id\":\"$DOING_COL\",\"position\":2}]}")
expect 200 "$CODE" "POST columns/reorder (restore)"
assert_eq "Todo" "$(jget '.[0].name')" "reorder restored Todo first"

step "GET /api/boards/:id/cards — assembled board view, no cards yet"
CODE=$(req GET "/api/boards/$BOARD_ID/cards"); expect 200 "$CODE" "GET board view"
assert_eq "$BOARD_NAME" "$(jget '.board.name')"       "board view board name"
assert_eq 4             "$(jget '.columns | length')" "board view column count"
assert_eq "Todo"        "$(jget '.columns[0].column.name')" "board view first column"
assert_eq 0             "$(jget '[.columns[].cards[]?] | length')" "board view card count"

step "WIP limit is enforced BEFORE noteboard is called (Blocked has wip_limit=0)"
CODE=$(req POST "/api/boards/$BOARD_ID/cards" \
  "{\"title\":\"should not fit\",\"column_id\":\"$BLOCKED_COL\"}")
expect 409 "$CODE" "POST card into a full column"
jget '.error' | grep -qi 'wip limit' || fail "409 body did not mention the WIP limit: $(cat "$BODY")"

step "noteboard-backed routes degrade to 502 (not a panic) while noteboard is down"
CODE=$(req POST "/api/boards/$BOARD_ID/cards" \
  "{\"title\":\"e2e card\",\"column_id\":\"$TODO_COL\"}")
expect 502 "$CODE" "POST card with noteboard unreachable"
jget '.error' | grep -qi 'noteboard create failed' \
  || fail "502 body did not name noteboard: $(cat "$BODY")"

CODE=$(req GET "/api/search?q=anything"); expect 502 "$CODE" "GET /api/search with noteboard unreachable"
CODE=$(req GET "/api/search"); expect 400 "$CODE" "GET /api/search with no q"

step "…and the server is still alive and serving after those failures"
CODE=$(req GET /health); expect 200 "$CODE" "/health after noteboard failures"
assert_eq 0 "$(jget '.counts.placements')" "no placement leaked from the failed card create"

step "POST /api/cards/:id/links — card links are pure kanban state (no noteboard)"
LINK_CARD="e2e-card-$$"
CODE=$(req POST "/api/cards/$LINK_CARD/links" \
  '{"entity_type":"session","entity_ref":"e2e-session-1","label":"spawned by"}')
expect 201 "$CODE" "POST card link"
LINK_ID="$(jget '.id')"
assert_eq session        "$(jget '.entity_type')" "link entity_type"
assert_eq e2e-session-1  "$(jget '.entity_ref')"  "link entity_ref"
assert_eq "spawned by"   "$(jget '.label')"       "link label"

CODE=$(req GET "/api/cards/$LINK_CARD/links"); expect 200 "$CODE" "GET card links"
assert_eq 1              "$(jget 'length')"        "card link count"
assert_eq e2e-session-1  "$(jget '.[0].entity_ref')" "read-back link entity_ref"

CODE=$(req POST "/api/cards/$LINK_CARD/links" '{"entity_type":"session"}')
expect 400 "$CODE" "POST card link with no entity_ref"

step "PUT /api/cards/:id/assignments/:principal — a failed principal-store check fails the write (502), never a silent row"
CODE=$(req PUT "/api/cards/$LINK_CARD/assignments/principal_000001")
expect 502 "$CODE" "PUT assignment with principal-store unreachable"
jget '.error' | grep -q 'principal-store check failed' \
  || fail "502 body did not name the principal-store check: $(cat "$BODY")"
CODE=$(req PUT "/api/cards/$LINK_CARD/assignments/not-a-principal")
expect 400 "$CODE" "PUT assignment with a malformed principal id"
CODE=$(req GET "/api/cards/$LINK_CARD/assignments"); expect 200 "$CODE" "GET card assignments"
assert_eq 0 "$(jget 'length')" "no assignment row leaked from the refused writes"
CODE=$(req DELETE "/api/cards/$LINK_CARD/assignments/principal_000001")
expect 404 "$CODE" "DELETE an assignment that was never made"
CODE=$(req GET "/api/assignments"); expect 400 "$CODE" "GET /api/assignments with no principal_id"
CODE=$(req GET "/api/assignments?principal_id=principal_000001"); expect 200 "$CODE" "GET /api/assignments reverse lookup"
assert_eq 0 "$(jget 'length')" "reverse lookup is empty"

step "POST /api/entities/:type/:ref/tags — cross-entity tagging"
CODE=$(req POST /api/entities/session/e2e-session-1/tags '{"tag":"e2e-tag"}')
expect 201 "$CODE" "POST entity tag"
assert_eq e2e-tag "$(jget '.tag')" "created entity tag"

CODE=$(req GET /api/entities/session/e2e-session-1/tags); expect 200 "$CODE" "GET entity tags"
assert_eq e2e-tag "$(jget '.[0].tag')" "read-back entity tag"

CODE=$(req GET "/api/tags"); expect 200 "$CODE" "GET /api/tags"
assert_eq 1 "$(jget "[.[] | select(.tag==\"e2e-tag\")] | length")" "e2e-tag in global tag list"
assert_eq 1 "$(jget "[.[] | select(.tag==\"e2e-tag\")] | .[0].count")" "e2e-tag count"

CODE=$(req GET "/api/tags?tag=e2e-tag&entity_type=session"); expect 200 "$CODE" "GET /api/tags?tag="
assert_eq e2e-session-1 "$(jget '.[0].entity_ref')" "reverse tag lookup entity_ref"

step "GET /api/entity-types — static registry answers"
CODE=$(req GET /api/entity-types); expect 200 "$CODE" "GET /api/entity-types"
[ "$(jget 'length')" -gt 0 ] || fail "/api/entity-types returned an empty registry"

step "404s and 405s are handled, not panics"
CODE=$(req GET "/api/boards/does-not-exist");       expect 404 "$CODE" "GET unknown board"
CODE=$(req GET "/api/columns/does-not-exist");      expect 404 "$CODE" "GET unknown column"
CODE=$(req DELETE "/api/links/does-not-exist");     expect 404 "$CODE" "DELETE unknown link"
CODE=$(req PUT "/api/boards/$BOARD_ID" '{}');       expect 405 "$CODE" "PUT /api/boards/:id"

step "GET /health — counts reflect exactly what phase A wrote"
CODE=$(req GET /health); expect 200 "$CODE" "/health end of phase A"
assert_eq 1 "$(jget '.counts.boards')"      "board count"
assert_eq 4 "$(jget '.counts.columns')"     "column count"
assert_eq 1 "$(jget '.counts.card_links')"  "card_link count"
assert_eq 0 "$(jget '.counts.card_assignments')" "card_assignment count"
assert_eq 1 "$(jget '.counts.entity_tags')" "entity_tag count"

stop_server

# ============================================================================
# PHASE B — stub noteboard, real card lifecycle.
# ============================================================================
step "start stub noteboard on :$NB_PORT"
# In-memory stand-in implementing ONLY the endpoints internal/noteboard/client.go
# calls: POST/GET/PATCH/DELETE /api/items[/:id], GET /api/search, and
# POST /api/items/query without filters or sort (the entity reverse lookup). Threading
# because GetItems fans out concurrent GETs. The real noteboard (:8191) is never
# touched.
cat >"$TMP_DIR/noteboard-stub.py" <<'PYEOF'
import json, sys, threading, uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs, unquote

ITEMS = {}
LOCK = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _send(self, code, obj):
        blob = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(blob)))
        self.end_headers()
        self.wfile.write(blob)

    def _read(self):
        n = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(n) or b"{}")

    def _item_id(self, path):
        prefix = "/api/items/"
        return unquote(path[len(prefix):]) if path.startswith(prefix) else None

    def do_POST(self):
        path = urlparse(self.path).path
        if path == "/api/items/query":
            q = self._read()
            with LOCK:
                found = [ITEMS[i] for i in q.get("ids", []) if i in ITEMS]
                missing = [i for i in q.get("ids", []) if i not in ITEMS]
            answer = {"total": len(found), "ids": [i["id"] for i in found], "missing_ids": missing}
            if q.get("include_items"):
                answer["items"] = found
            return self._send(200, answer)
        if path == "/api/items/batch":
            p = self._read()
            if set(p) != {"ids"} or not isinstance(p["ids"], list):
                return self._send(400, {"error": "ids is required"})
            with LOCK:
                items = []
                for item_id in p["ids"]:
                    item = ITEMS.get(item_id)
                    items.append(dict(item) if item and not item.get("deleted_at") else None)
            return self._send(200, {"items": items})
        if path != "/api/items":
            return self._send(404, {"error": "not found"})
        p = self._read()
        if not p.get("title"):
            return self._send(400, {"error": "title is required"})
        item = {
            "id": str(uuid.uuid4()),
            "type": p.get("type", "todo"),
            "title": p["title"],
            "body": p.get("body"),
            "tags": p.get("tags") or [],
            "priority": p.get("priority"),
            "status": "open",
        }
        with LOCK:
            ITEMS[item["id"]] = item
        self._send(201, item)

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/api/search":
            q = (parse_qs(u.query).get("q") or [""])[0].lower()
            with LOCK:
                hits = [i for i in ITEMS.values() if q in i["title"].lower()]
            return self._send(200, hits)
        cid = self._item_id(u.path)
        if cid is None:
            return self._send(404, {"error": "not found"})
        with LOCK:
            item = ITEMS.get(cid)
        self._send(200, item) if item else self._send(404, {"error": "not found"})

    def do_PATCH(self):
        cid = self._item_id(urlparse(self.path).path)
        patch = self._read()
        if cid is None:
            return self._send(404, {"error": "not found"})
        with LOCK:
            item = ITEMS.get(cid)
            if not item:
                return self._send(404, {"error": "not found"})
            item.update(patch)
            snapshot = dict(item)
        self._send(200, snapshot)

    def do_DELETE(self):
        u = urlparse(self.path)
        cid = self._item_id(u.path)
        if cid is None:
            return self._send(404, {"error": "not found"})
        hard = parse_qs(u.query).get("hard") == ["true"]
        with LOCK:
            item = ITEMS.get(cid)
            if not item:
                return self._send(404, {"error": "not found"})
            if hard:
                del ITEMS[cid]
            else:
                item["status"] = "archived"
        self._send(200, {"status": "ok"})


ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PYEOF
python3 "$TMP_DIR/noteboard-stub.py" "$NB_PORT" >"$TMP_DIR/noteboard-stub.log" 2>&1 &
NB_PID=$!
NB_OK=""
for _ in $(seq 1 50); do
  if ! kill -0 "$NB_PID" 2>/dev/null; then fail "stub noteboard exited during startup"; fi
  if curl -fsS --max-time 2 -o /dev/null "$NB_BASE/api/search?q=ping" 2>/dev/null; then NB_OK=1; break; fi
  sleep 0.1
done
[ -n "$NB_OK" ] || fail "stub noteboard did not come up on $NB_BASE"
echo "    pid: $NB_PID  ready"

start_server "$NB_BASE" "noteboard STUB"

step "POST /api/boards/:id/cards — create a card in Todo"
CARD_TITLE="e2e smoke card $$"
CODE=$(req POST "/api/boards/$BOARD_ID/cards" \
  "{\"title\":\"$CARD_TITLE\",\"column_id\":\"$TODO_COL\",\"tags\":[\"e2e\"],\"priority\":2}")
expect 201 "$CODE" "POST card"
CARD_ID="$(jget '.item.id')"
[ -n "$CARD_ID" ] && [ "$CARD_ID" != "null" ] || fail "POST card returned no item id"
assert_eq "$CARD_TITLE" "$(jget '.item.title')"          "created card title"
assert_eq open          "$(jget '.item.status')"         "created card status (Todo has no auto_status)"
assert_eq "$TODO_COL"   "$(jget '.placement.column_id')" "created card placement column"
assert_eq "$BOARD_ID"   "$(jget '.placement.board_id')"  "created card placement board"
echo "    card id: $CARD_ID"

step "GET /api/boards/:id/cards — the card comes back through board-view assembly"
CODE=$(req GET "/api/boards/$BOARD_ID/cards"); expect 200 "$CODE" "GET board view with a card"
assert_eq 1 "$(jget '[.columns[].cards[]?] | length')" "board view card count"
assert_eq "$CARD_TITLE" \
  "$(jget ".columns[] | select(.column.id==\"$TODO_COL\") | .cards[0].item.title")" \
  "card title in the Todo column of the board view"
assert_eq "$CARD_ID" \
  "$(jget ".columns[] | select(.column.id==\"$TODO_COL\") | .cards[0].placement.card_id")" \
  "card id in the Todo column of the board view"
assert_eq 0 "$(jget '.orphans | length')" "no orphaned cards"

step "POST /api/cards/:id/move — move to Done, auto_status writes back to noteboard"
CODE=$(req POST "/api/cards/$CARD_ID/move" \
  "{\"board_id\":\"$BOARD_ID\",\"column_id\":\"$DONE_COL\",\"position\":1}")
expect 200 "$CODE" "POST card move"
assert_eq "$DONE_COL" "$(jget '.placement.column_id')"  "moved card column"
assert_eq done        "$(jget '.auto_status_applied')"  "auto_status applied on move"
[ "$(jget '.auto_status_error // "none"')" = "none" ] || fail "move reported auto_status_error: $(cat "$BODY")"

# The side effect is only real if the noteboard item itself changed.
NB_STATUS="$(curl -fsS --max-time 5 "$NB_BASE/api/items/$CARD_ID" | jq -r '.status')"
assert_eq done "$NB_STATUS" "noteboard item status after move into an auto_status=done column"

step "GET /api/cards/:id/placements — the card knows where it lives"
CODE=$(req GET "/api/cards/$CARD_ID/placements"); expect 200 "$CODE" "GET placements"
assert_eq 1           "$(jget 'length')"          "placement count"
assert_eq "$DONE_COL" "$(jget '.[0].column_id')"  "placement column after move"

step "GET /api/entities/:type/:ref/cards — reverse lookup joins noteboard"
CODE=$(req POST "/api/cards/$CARD_ID/links" \
  '{"entity_type":"session","entity_ref":"e2e-session-1","label":"worked by"}')
expect 201 "$CODE" "POST link on the real card"
CODE=$(req GET /api/entities/session/e2e-session-1/cards); expect 200 "$CODE" "GET entity cards"
assert_eq "$CARD_TITLE" \
  "$(jget ".[] | select(.card_id==\"$CARD_ID\") | .item.title")" \
  "entity reverse-lookup returned the card's noteboard content"
assert_eq done "$(jget ".[] | select(.card_id==\"$CARD_ID\") | .work_state")" \
  "entity reverse-lookup carries the card's shared work_state (last moved into Done)"
# The phase-A link points at a card id that has no noteboard item — it must come
# back as a null item (orphan), not blow up the whole response.
assert_eq null "$(jget ".[] | select(.card_id==\"$LINK_CARD\") | .item")" \
  "orphaned card link surfaces as item:null"

step "GET /api/search — delegated FTS, filtered to cards on this board"
CODE=$(req GET "/api/search?q=$(printf '%s' "$CARD_TITLE" | jq -sRr @uri)&board_id=$BOARD_ID")
expect 200 "$CODE" "GET /api/search?board_id="
assert_eq "$CARD_ID" "$(jget '.[0].id')" "search returned the card on this board"

step "DELETE /api/boards/:id/cards/:cardID — detach, then PUT to re-attach"
CODE=$(req DELETE "/api/boards/$BOARD_ID/cards/$CARD_ID"); expect 200 "$CODE" "DELETE placement"
CODE=$(req GET "/api/boards/$BOARD_ID/cards"); expect 200 "$CODE" "GET board view after detach"
assert_eq 0 "$(jget '[.columns[].cards[]?] | length')" "board view empty after detach"

CODE=$(req PUT "/api/boards/$BOARD_ID/cards/$CARD_ID" "{\"column_id\":\"$TODO_COL\"}")
expect 201 "$CODE" "PUT re-attach existing card"
assert_eq "$TODO_COL" "$(jget '.column_id')" "re-attached card column"

CODE=$(req PUT "/api/boards/$BOARD_ID/cards/no-such-noteboard-item" "{\"column_id\":\"$TODO_COL\"}")
expect 404 "$CODE" "PUT attach a card that does not exist in noteboard"

step "PATCH /api/cards/:id — edits forward to noteboard"
CODE=$(req PATCH "/api/cards/$CARD_ID" '{"title":"e2e retitled"}')
expect 200 "$CODE" "PATCH card"
assert_eq "e2e retitled" "$(jget '.title')" "patched card title"
NB_TITLE="$(curl -fsS --max-time 5 "$NB_BASE/api/items/$CARD_ID" | jq -r '.title')"
assert_eq "e2e retitled" "$NB_TITLE" "noteboard item title after PATCH"

step "DELETE /api/boards/:id — cascade drops columns and placements"
CODE=$(req DELETE "/api/boards/$BOARD_ID"); expect 200 "$CODE" "DELETE board"
CODE=$(req GET "/api/boards/$BOARD_ID"); expect 404 "$CODE" "GET board after delete"
CODE=$(req GET /health); expect 200 "$CODE" "/health after cascade"
assert_eq 0 "$(jget '.counts.boards')"     "boards after cascade delete"
assert_eq 0 "$(jget '.counts.columns')"    "columns after cascade delete"
assert_eq 0 "$(jget '.counts.placements')" "placements after cascade delete"

step "SUCCESS — kanban-store boots and answers from this tree"
echo "    server log: $TMP_DIR/server.log"
