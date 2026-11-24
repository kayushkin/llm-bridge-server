#!/usr/bin/env bash
# Boot-and-answer smoke test for permission-store.
#
# Builds the service from THIS checkout, boots it against a throwaway SQLite
# database on a throwaway port, and drives the real HTTP surface: rule CRUD,
# the scope cascade, the Bash splitter, and the audit log. Proves the binary
# does more than compile — a Go 1.22+ http.ServeMux panics on a conflicting
# route pattern at *registration* time, which no compiler catches. That class
# of dead-binary bug is exactly what this catches: if routes() panics, the
# health poll below never succeeds and the run fails loudly.
#
# HERMETIC BY CONSTRUCTION. permission-store gates tool calls for live agent
# sessions, so nothing here may reach the live instance (127.0.0.1:8304, db
# ~/.config/permission-store/permission-store.db):
#   * --db points at a temp file, and HOME is redirected into the temp dir as
#     a second line of defence — permstore.DefaultPath() is $HOME-relative, so
#     even a hypothetical bug that ignored --db still could not find the live
#     database.
#   * --port is a temp port, and the run aborts up front if it is the live one.
#   * /healthz echoes the db path it actually opened; we assert it is ours.
#   * The service makes no outbound calls at all (no os.Getenv, no http client
#     anywhere in the tree), so there is no sibling dependency to stub out.
#
# Exits 0 on success, non-zero on the first failing assertion. On failure the
# server log is dumped to stderr.
#
# Tunables:
#   E2E_PORT — listen port (default 19111)
#   E2E_KEEP — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19111}"
BASE="http://127.0.0.1:$PORT"

# The live deployment, read from permission-store.service. Belt-and-braces:
# refuse to run if someone points E2E_PORT at the production listener.
LIVE_PORT=8304
if [ "$PORT" = "$LIVE_PORT" ]; then
  echo "ERROR: E2E_PORT=$PORT is the LIVE permission-store port — refusing to run" >&2
  exit 2
fi

for bin in go curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t permission-store-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
FAKE_HOME="$TMP_DIR/home"
DB_PATH="$DATA_DIR/permission-store.db"
SERVER_LOG="$TMP_DIR/server.log"
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
fail() {
  echo "FAIL: $*" >&2
  if [ -s "$SERVER_LOG" ]; then
    echo "----- server.log -----" >&2
    cat "$SERVER_LOG" >&2
    echo "----------------------" >&2
  fi
  exit 1
}

# code <method> <path> [json-body] → bare HTTP status, for the 4xx/204 asserts
# where curl -f would just abort.
code() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS -o /dev/null -w '%{http_code}' --max-time 10 \
      -X "$method" "$BASE$path" -H 'Content-Type: application/json' -d "$body"
  else
    curl -sS -o /dev/null -w '%{http_code}' --max-time 10 -X "$method" "$BASE$path"
  fi
}

step "build binaries from $REPO_DIR"
cd "$REPO_DIR"
go build -o "$BIN_DIR/permission-store" ./cmd/permission-store
go build -o "$BIN_DIR/probe" ./cmd/probe
echo "    permission-store: $(ls -lh "$BIN_DIR/permission-store" | awk '{print $5}')"
echo "    probe:            $(ls -lh "$BIN_DIR/probe" | awk '{print $5}')"

step "launch on :$PORT (db: $DB_PATH)"
# HOME is redirected so DefaultPath() can only ever resolve inside $TMP_DIR.
HOME="$FAKE_HOME" \
  "$BIN_DIR/permission-store" --port "$PORT" --db "$DB_PATH" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

# Poll — never sleep-and-hope. ~10s budget, then fail loudly. A route-pattern
# panic in routes() dies here rather than shipping a dead binary.
READY=""
for _ in $(seq 1 50); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    fail "server process exited during startup (route panic? port in use?)"
  fi
  if curl -fsS --max-time 2 "$BASE/healthz" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.2
done
[ -n "$READY" ] || fail "server did not answer $BASE/healthz within ~10s"

step "GET /healthz — status ok, and the db it opened is OURS (not the live one)"
HEALTH=$(curl -fsS --max-time 10 "$BASE/healthz")
echo "    $HEALTH"
[ "$(jq -r '.status' <<<"$HEALTH")" = "ok" ] || fail "healthz status not ok: $HEALTH"
OPENED_DB=$(jq -r '.db' <<<"$HEALTH")
[ "$OPENED_DB" = "$DB_PATH" ] || fail "server opened $OPENED_DB, expected the temp db $DB_PATH"
case "$OPENED_DB" in
  "$TMP_DIR"/*) ;;
  *) fail "server db $OPENED_DB escaped the temp dir — ABORTING before it can touch live state" ;;
esac
[ -f "$DB_PATH" ] || fail "temp db was never created at $DB_PATH"

step "GET /rules — migrateV2 seeded the default rule set"
RULES=$(curl -fsS --max-time 10 "$BASE/rules")
N=$(jq 'length' <<<"$RULES")
echo "    $N seed rules"
[ "$N" -ge 15 ] || fail "expected >=15 seeded rules on a fresh db, got $N"
jq -e '.[] | select(.id=="seed-v1-allow-Read" and .outcome=="allow" and .enabled==true)' \
  <<<"$RULES" >/dev/null || fail "seed-v1-allow-Read missing / not an enabled allow"

step "GET /rules/seed-v1-deny-bash-rm-rf-abs — parsed body carries the real fields"
SEED=$(curl -fsS --max-time 10 "$BASE/rules/seed-v1-deny-bash-rm-rf-abs")
[ "$(jq -r '.outcome' <<<"$SEED")" = "deny" ]   || fail "rm -rf seed is not a deny: $SEED"
[ "$(jq -r '.tool'    <<<"$SEED")" = "Bash" ]   || fail "rm -rf seed is not scoped to Bash: $SEED"
[ "$(jq -r '.scope'   <<<"$SEED")" = "global" ] || fail "rm -rf seed is not global: $SEED"
echo "    pattern: $(jq -r '.pattern' <<<"$SEED")"

step "POST /evaluate — Bash 'rm -rf /tmp/x' hits the deny seed"
EV=$(curl -fsS --max-time 10 -X POST "$BASE/evaluate" -H 'Content-Type: application/json' \
  -d '{"tool":"Bash","input":{"command":"rm -rf /tmp/x"}}')
[ "$(jq -r '.outcome' <<<"$EV")" = "deny" ] || fail "rm -rf /tmp/x was not denied: $EV"
[ "$(jq -r '.matched_rule_id' <<<"$EV")" = "seed-v1-deny-bash-rm-rf-abs" ] \
  || fail "deny came from the wrong rule: $EV"
jq -e '.message | test("absolute path")' <<<"$EV" >/dev/null \
  || fail "deny did not surface the seed's message: $EV"
echo "    deny — $(jq -r '.message' <<<"$EV")"

step "POST /evaluate — Bash 'git status' hits the read-only allow seed"
EV=$(curl -fsS --max-time 10 -X POST "$BASE/evaluate" -H 'Content-Type: application/json' \
  -d '{"tool":"Bash","input":{"command":"git status"}}')
[ "$(jq -r '.outcome' <<<"$EV")" = "allow" ] || fail "git status was not allowed: $EV"
[ "$(jq -r '.matched_rule_id' <<<"$EV")" = "seed-v1-allow-bash-git-readonly" ] \
  || fail "allow came from the wrong rule: $EV"

step "POST /evaluate — splitter splits a compound command, deny short-circuits"
# Exercises splitter.Split through the HTTP surface: two atoms, and the deny
# on the second one wins over the allow on the first.
EV=$(curl -fsS --max-time 10 -X POST "$BASE/evaluate" -H 'Content-Type: application/json' \
  -d '{"tool":"Bash","input":{"command":"git status && rm -rf /etc"}}')
ATOMS=$(jq '.atoms | length' <<<"$EV")
echo "    atoms: $ATOMS  outcome: $(jq -r '.outcome' <<<"$EV")"
[ "$ATOMS" -eq 2 ] || fail "expected 2 atoms from 'git status && rm -rf /etc', got $ATOMS: $EV"
jq -e '[.atoms[].line] | index("rm -rf /etc")' <<<"$EV" >/dev/null \
  || fail "splitter did not surface the 'rm -rf /etc' atom: $EV"
[ "$(jq -r '.outcome' <<<"$EV")" = "deny" ] || fail "compound command with a deny atom was not denied: $EV"

step "POST /evaluate — unmatched tool falls through to the safe default (ask)"
EV=$(curl -fsS --max-time 10 -X POST "$BASE/evaluate" -H 'Content-Type: application/json' \
  -d '{"tool":"E2eSmokeUnknownTool","input":{"x":1}}')
[ "$(jq -r '.outcome' <<<"$EV")" = "ask" ] || fail "unknown tool did not default to ask: $EV"
[ "$(jq -r '.matched_rule_id' <<<"$EV")" = "default-ask" ] || fail "expected default-ask sentinel: $EV"

step "POST /rules — write a bridge-scoped rule, then read it back"
BRIDGE_ID="e2e-smoke-$$"
CREATED=$(curl -fsS --max-time 10 -X POST "$BASE/rules" -H 'Content-Type: application/json' \
  -d "{\"scope\":\"bridge:$BRIDGE_ID\",\"priority\":10,\"tool\":\"Bash\",\"pattern\":\"^e2e-canary\\\\b\",\"outcome\":\"allow\",\"message\":\"e2e-smoke canary rule\",\"enabled\":true}")
RULE_ID=$(jq -r '.id' <<<"$CREATED")
[ -n "$RULE_ID" ] && [ "$RULE_ID" != "null" ] || fail "POST /rules returned no id: $CREATED"
echo "    rule id: $RULE_ID"

# Read-back through a SEPARATE request — proves it actually persisted to
# SQLite, not just that the handler echoed our payload.
GOT=$(curl -fsS --max-time 10 "$BASE/rules/$RULE_ID")
[ "$(jq -r '.scope'    <<<"$GOT")" = "bridge:$BRIDGE_ID" ]     || fail "scope not persisted: $GOT"
[ "$(jq -r '.tool'     <<<"$GOT")" = "Bash" ]                  || fail "tool not persisted: $GOT"
[ "$(jq -r '.pattern'  <<<"$GOT")" = '^e2e-canary\b' ]         || fail "pattern not persisted: $GOT"
[ "$(jq -r '.outcome'  <<<"$GOT")" = "allow" ]                 || fail "outcome not persisted: $GOT"
[ "$(jq -r '.message'  <<<"$GOT")" = "e2e-smoke canary rule" ] || fail "message not persisted: $GOT"
[ "$(jq -r '.enabled'  <<<"$GOT")" = "true" ]                  || fail "enabled not persisted: $GOT"
[ "$(jq -r '.priority' <<<"$GOT")" = "10" ]                    || fail "priority not persisted: $GOT"
[ -n "$(jq -r '.created_at' <<<"$GOT")" ]                      || fail "created_at not stamped: $GOT"

step "POST /evaluate — the rule we just wrote governs its own bridge"
EV=$(curl -fsS --max-time 10 -X POST "$BASE/evaluate" -H 'Content-Type: application/json' \
  -d "{\"bridge_id\":\"$BRIDGE_ID\",\"tool\":\"Bash\",\"input\":{\"command\":\"e2e-canary go\"}}")
[ "$(jq -r '.outcome' <<<"$EV")" = "allow" ] || fail "canary command not allowed for its bridge: $EV"
[ "$(jq -r '.matched_rule_id' <<<"$EV")" = "$RULE_ID" ] \
  || fail "expected our rule $RULE_ID to match, got: $EV"

step "POST /evaluate — and ONLY its own bridge (scope cascade isolates it)"
EV=$(curl -fsS --max-time 10 -X POST "$BASE/evaluate" -H 'Content-Type: application/json' \
  -d '{"bridge_id":"some-other-bridge","tool":"Bash","input":{"command":"e2e-canary go"}}')
[ "$(jq -r '.outcome' <<<"$EV")" = "ask" ] \
  || fail "bridge-scoped rule leaked into another bridge: $EV"
[ "$(jq -r '.matched_rule_id' <<<"$EV")" = "default-ask" ] || fail "expected default-ask for other bridge: $EV"

step "PATCH /rules/$RULE_ID — flip allow→deny, engine picks it up immediately"
PATCHED=$(curl -fsS --max-time 10 -X PATCH "$BASE/rules/$RULE_ID" -H 'Content-Type: application/json' \
  -d '{"outcome":"deny","message":"canary is now denied"}')
[ "$(jq -r '.outcome' <<<"$PATCHED")" = "deny" ] || fail "PATCH did not apply outcome: $PATCHED"
# PATCH semantics: untouched fields must survive the merge.
[ "$(jq -r '.pattern' <<<"$PATCHED")" = '^e2e-canary\b' ] || fail "PATCH wiped the pattern: $PATCHED"
[ "$(jq -r '.tool'    <<<"$PATCHED")" = "Bash" ]          || fail "PATCH wiped the tool: $PATCHED"

EV=$(curl -fsS --max-time 10 -X POST "$BASE/evaluate" -H 'Content-Type: application/json' \
  -d "{\"bridge_id\":\"$BRIDGE_ID\",\"tool\":\"Bash\",\"input\":{\"command\":\"e2e-canary go\"}}")
[ "$(jq -r '.outcome' <<<"$EV")" = "deny" ] || fail "engine did not see the PATCHed rule (stale cache?): $EV"
[ "$(jq -r '.message' <<<"$EV")" = "canary is now denied" ] || fail "PATCHed message not surfaced: $EV"
echo "    deny — $(jq -r '.message' <<<"$EV")"

step "GET /rules?scope=bridge:$BRIDGE_ID — scope filter returns exactly our rule"
SCOPED=$(curl -fsS --max-time 10 "$BASE/rules?scope=bridge:$BRIDGE_ID")
[ "$(jq 'length' <<<"$SCOPED")" -eq 1 ]           || fail "scope filter returned $(jq 'length' <<<"$SCOPED") rules, want 1: $SCOPED"
[ "$(jq -r '.[0].id' <<<"$SCOPED")" = "$RULE_ID" ] || fail "scope filter returned the wrong rule: $SCOPED"

step "GET /audit?bridge_id=$BRIDGE_ID — every evaluate was logged"
AUDIT=$(curl -fsS --max-time 10 "$BASE/audit?bridge_id=$BRIDGE_ID")
NA=$(jq 'length' <<<"$AUDIT")
echo "    $NA audit entries"
# Two evaluates carried this bridge_id: the allow, then the post-PATCH deny.
[ "$NA" -eq 2 ] || fail "expected 2 audit entries for $BRIDGE_ID, got $NA: $AUDIT"
jq -e --arg id "$RULE_ID" '[.[] | select(.outcome=="deny" and .matched_rule_id==$id)] | length == 1' \
  <<<"$AUDIT" >/dev/null || fail "audit log missing the deny verdict for $RULE_ID: $AUDIT"
jq -e '.[0].tool == "Bash" and (.[0].ts | length > 0)' <<<"$AUDIT" >/dev/null \
  || fail "audit entry is missing tool / ts: $AUDIT"
jq -e '[.[] | select(.atoms != null)] | length == 2' <<<"$AUDIT" >/dev/null \
  || fail "audit entries did not persist the split atoms: $AUDIT"

step "POST /rules — validation rejects a bad outcome (400)"
C=$(code POST /rules '{"scope":"global","tool":"Bash","outcome":"maybe"}')
[ "$C" = "400" ] || fail "bad outcome should be 400, got $C"
C=$(code POST /rules '{"scope":"global","outcome":"allow"}')
[ "$C" = "400" ] || fail "missing tool should be 400, got $C"
C=$(code POST /evaluate '{"input":{}}')
[ "$C" = "400" ] || fail "evaluate without a tool should be 400, got $C"

step "GET /rules/<missing> — 404"
C=$(code GET /rules/e2e-no-such-rule)
[ "$C" = "404" ] || fail "missing rule should be 404, got $C"

step "DELETE /rules/$RULE_ID — 204, and it is really gone"
C=$(code DELETE "/rules/$RULE_ID")
[ "$C" = "204" ] || fail "DELETE should be 204, got $C"
C=$(code GET "/rules/$RULE_ID")
[ "$C" = "404" ] || fail "deleted rule should now 404, got $C"
# ...and the engine stops honoring it.
EV=$(curl -fsS --max-time 10 -X POST "$BASE/evaluate" -H 'Content-Type: application/json' \
  -d "{\"bridge_id\":\"$BRIDGE_ID\",\"tool\":\"Bash\",\"input\":{\"command\":\"e2e-canary go\"}}")
[ "$(jq -r '.matched_rule_id' <<<"$EV")" = "default-ask" ] \
  || fail "deleted rule is still matching: $EV"

step "hermeticity — --db was honored, DefaultPath() never used"
# The server ran with HOME=$FAKE_HOME, so DefaultPath() would have landed here
# had --db been ignored. Its absence proves every write went to the temp db.
[ ! -e "$FAKE_HOME/.config/permission-store/permission-store.db" ] \
  || fail "server created a db at DefaultPath() — --db was ignored"
echo "    ok — every write landed in $DB_PATH"

step "SUCCESS"
echo "    all routes answered: /healthz /rules /rules/{id} /evaluate /audit"
echo "    server log: $SERVER_LOG"
