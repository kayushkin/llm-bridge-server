#!/usr/bin/env bash
# Boot-and-answer smoke test for mailstack.
#
# Builds the server from THIS checkout, boots it against a throwaway accounts DB
# on a throwaway port, and drives the account lifecycle through real HTTP:
# create, read back, list, patch, delete. The live service (:8195) and the live
# accounts DB are never touched.
#
# ------------------------------------------------------------------------
# THE CREDENTIAL HAZARD — read before changing the environment below.
#
# mailstack is the one service in this batch that can reach the user's actual
# mail. Two things keep this run away from it, and both are asserted, not
# assumed:
#
#   1. AUTH_STORE_URL points at a CLOSED PORT. mailstack pulls its OAuth tokens
#      from auth-store (:8303) and would otherwise hold live Gmail credentials.
#      With the store unreachable, the Gmail backend cannot mint a token, and
#      the assertion below requires that it fails with a 502 — i.e. it degrades
#      loudly at the edge instead of silently reaching for real mail.
#   2. MAILSTACK_DB points into the temp dir. accounts.DefaultDBPath() resolves
#      to $HOME/.config/mailstack/accounts.db — so if that override is ever
#      dropped, this smoke would open the LIVE account list, which is both a
#      lie (every assertion below would be measuring live data) and a write.
#      The hermeticity step at the end fails if that default path was created.
#      MAILSTACK_LOCAL_DB does the same for the synthetic-mail database.
#
# No IMAP or Gmail connection is ever attempted against a real host: the only
# account this smoke creates is one it invents, and the only backend it drives
# is the one whose credential source it has just disconnected.
# ------------------------------------------------------------------------
#
# The boot-time failure class the compiler cannot see is route registration:
# NewServer() registers twenty patterns on an http.ServeMux, many of them
# overlapping ("GET /api/messages" vs "GET /api/messages/{id}"), and Go 1.22+
# ServeMux panics on a CONFLICTING pattern at registration time — green under
# `go build` and `go vet`, dead at boot. Every route below has to resolve.
#
# The bearer gate is driven through BOTH branches on purpose: a gate that never
# closes looks exactly like a working one if you only ever send the token.
#
# Exits 0 on success, non-zero on the first failing assertion. On failure the
# server log is dumped to stderr.
#
# Tunables:
#   E2E_PORT  — listen port (default 19124; NOT the live 8195)
#   E2E_KEEP  — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19124}"
BASE="http://127.0.0.1:$PORT"
TOKEN="e2e-smoke-bearer-token"
# A closed port on loopback. This is what stands between the run and the user's
# real OAuth tokens.
DEAD_AUTH_STORE="http://127.0.0.1:1"

for bin in go curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t mailstack-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DB_PATH="$TMP_DIR/data/accounts.db"
LOG="$TMP_DIR/server.log"
mkdir -p "$BIN_DIR" "$TMP_DIR/data"

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
  # log here (not only in fail()) is what covers the abort paths `set -e` takes
  # on its own, e.g. a `curl -fsS` returning non-2xx inside a $(...) assignment.
  local status=$?
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  [ "$status" -ne 0 ] && dump_log
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

auth() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }

# Refuse to run if something already owns the port — otherwise every assertion
# below would silently be testing THAT process (e.g. the live service).
if curl -fsS -o /dev/null --max-time 2 "$BASE/health" 2>/dev/null; then
  echo "ERROR: something is already listening on $BASE — set E2E_PORT" >&2
  exit 2
fi

step "build mailstack from $REPO_DIR"
cd "$REPO_DIR"
CGO_ENABLED=0 go build -o "$BIN_DIR/mailstack" ./cmd/mailstack
echo "    binary: $BIN_DIR/mailstack ($(ls -lh "$BIN_DIR/mailstack" | awk '{print $5}'))"

step "launch mailstack on :$PORT (db: $DB_PATH, auth-store: DISCONNECTED)"
MAILSTACK_PORT="$PORT" \
MAILSTACK_DB="$DB_PATH" \
MAILSTACK_LOCAL_DB="$TMP_DIR/data/local-mail.db" \
MAILSTACK_TOKEN="$TOKEN" \
AUTH_STORE_URL="$DEAD_AUTH_STORE" \
AUTH_STORE_TOKEN="" \
  "$BIN_DIR/mailstack" >"$LOG" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

# Poll for readiness — never sleep-and-hope. The store has to open SQLite and
# migrate before the listener binds. Bail the instant the process dies, so a
# route-registration panic surfaces as a failure and not a mystery timeout.
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

step "GET /health — unauthenticated, by design"
HEALTH="$(curl -fsS "$BASE/health")"
echo "    $(tr -d '\n ' <<<"$HEALTH")"
# handleHealth answers {"ok": true} — not the {"status":"ok"} shape the other
# services in this fleet use. Assert what this server actually says.
[ "$(jq -r '.ok' <<<"$HEALTH")" = "true" ] || fail "/health is not ok: $HEALTH"

step "GET /api/accounts WITHOUT a token — the gate must close"
# Drive the closed branch first. A gate that never closes is indistinguishable
# from a working one if the smoke only ever sends the token.
CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/api/accounts")"
[ "$CODE" = "401" ] || fail "GET /api/accounts with no bearer token returned $CODE, want 401"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer wrong-token" "$BASE/api/accounts")"
[ "$CODE" = "401" ] || fail "GET /api/accounts with a WRONG bearer token returned $CODE, want 401"
echo "    401 with no token and with a wrong token"

step "GET /settings — behind the same gate, and it never serves a token"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/settings")"
[ "$CODE" = "401" ] || fail "GET /settings with no bearer token returned $CODE, want 401"
SETTINGS="$(auth "$BASE/settings")"
[ "$(jq -r '.service' <<<"$SETTINGS")" = "mailstack" ] || fail "/settings does not describe mailstack: $SETTINGS"
# The port this run was started on reached the registry, and came from the
# environment. A registry built but never read from would serve the default.
[ "$(jq -r '.settings[] | select(.key == "listen_port") | .value' <<<"$SETTINGS")" = "$PORT" ] \
  || fail "/settings does not show the port this server listens on: $SETTINGS"
if grep -qF "$TOKEN" <<<"$SETTINGS"; then
  fail "/settings serves the bearer token's text"
fi
echo "    401 with no token; 200 with it; port $PORT shown; the token's text is not in the body"

step "GET /api/accounts WITH the token — and the DB is empty"
LIST0="$(auth "$BASE/api/accounts")"
echo "    $LIST0"
# A fresh DB has no accounts. Anything else means we opened a populated one —
# i.e. very possibly the user's live account list, and every assertion below
# would be measuring their data.
# The list route wraps its payload: {"accounts": [...]}, and an empty list comes
# back as null, not []. `jq 'length'` on the OBJECT would count its keys and
# report 1 for an empty board — assert on .accounts, where `null | length` is 0.
[ "$(jq -r '.accounts | length' <<<"$LIST0")" = "0" ] \
  || fail "expected an empty temp accounts DB, got $(jq -r '.accounts | length' <<<"$LIST0") accounts — is this the LIVE db?"

step "POST /api/accounts — validation rejects an incomplete body"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/api/accounts" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"only-an-id"}')"
[ "$CODE" = "400" ] \
  || fail "POST /api/accounts with no label/provider/address returned $CODE, want 400 (404 = route not registered)"
echo "    400 as expected"

step "POST /api/accounts — create"
ACCT_ID="e2e-smoke-account"
CREATED="$(auth -X POST "$BASE/api/accounts" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$ACCT_ID\",\"label\":\"E2E Smoke\",\"provider\":\"google\",\"address\":\"e2e@example.invalid\"}")"
echo "    $CREATED"
[ "$(jq -r '.id' <<<"$CREATED")" = "$ACCT_ID" ] || fail "created account has the wrong id: $CREATED"
# Both of these are server-side defaults, not echoes of what we sent — they
# prove handleCreateAccount actually ran rather than reflecting the body back.
[ "$(jq -r '.enabled' <<<"$CREATED")" = "true" ] || fail "new account should default to enabled: $CREATED"
[ "$(jq -r '.color' <<<"$CREATED")" = "#888" ] || fail "new account should default to color #888: $CREATED"

step "GET /api/accounts/$ACCT_ID — read back what we wrote"
GOT="$(auth "$BASE/api/accounts/$ACCT_ID")"
[ "$(jq -r '.label'    <<<"$GOT")" = "E2E Smoke" ]           || fail "label did not round-trip: $GOT"
[ "$(jq -r '.provider' <<<"$GOT")" = "google" ]              || fail "provider did not round-trip: $GOT"
[ "$(jq -r '.address'  <<<"$GOT")" = "e2e@example.invalid" ] || fail "address did not round-trip: $GOT"
[ "$(auth "$BASE/api/accounts" | jq -r '.accounts | length')" = "1" ] \
  || fail "the list route does not show the new account"
echo "    label/provider/address round-tripped, and it is in the list"

step "GET /api/messages — the Gmail backend degrades LOUDLY with auth-store down"
# This is the credential assertion. The account above is a `google` account, so
# listing its messages needs an OAuth token, and the only source of one is
# auth-store — which is pointed at a closed port. Requirements: a 502 (not a
# 200, not a hang), promptly, and the server still alive afterwards.
#
# A 200 here would be alarming: it would mean the binary found real credentials
# from somewhere other than the auth-store URL it was given.
MSG_CODE="$(curl -sS -o "$TMP_DIR/messages.out" -w '%{http_code}' --max-time 20 \
  -H "Authorization: Bearer $TOKEN" "$BASE/api/messages?account=$ACCT_ID")"
[ "$MSG_CODE" != "200" ] \
  || fail "GET /api/messages returned 200 with auth-store unreachable — where did it get a token? $(cat "$TMP_DIR/messages.out")"
[ "$MSG_CODE" = "502" ] \
  || fail "expected 502 from the Gmail backend with auth-store down, got $MSG_CODE: $(cat "$TMP_DIR/messages.out")"
echo "    502: $(head -c 120 "$TMP_DIR/messages.out")"

step "PATCH /api/accounts/$ACCT_ID — update persists"
auth -X PATCH "$BASE/api/accounts/$ACCT_ID" -H 'Content-Type: application/json' \
  -d '{"label":"E2E Smoke Renamed"}' >/dev/null \
  || fail "PATCH /api/accounts/$ACCT_ID did not answer 2xx"
# Re-read rather than trusting the handler's echo — the write has to be durable.
[ "$(auth "$BASE/api/accounts/$ACCT_ID" | jq -r '.label')" = "E2E Smoke Renamed" ] \
  || fail "the PATCHed label did not persist to the DB"
echo "    label updated and persisted"

step "DELETE /api/accounts/$ACCT_ID"
auth -X DELETE "$BASE/api/accounts/$ACCT_ID" >/dev/null || fail "DELETE did not answer 2xx"
[ "$(auth "$BASE/api/accounts" | jq -r '.accounts | length')" = "0" ] \
  || fail "the account is still listed after DELETE"
echo "    deleted; account list back to empty"

step "the run never went near the live credentials"
[ -f "$DB_PATH" ] || fail "temp DB $DB_PATH was never created — which database did we just write to?"
# accounts.DefaultDBPath() is $HOME/.config/mailstack/accounts.db. The audit
# harness redirects HOME, so this is the assertion that catches a DROPPED
# MAILSTACK_DB override: without it, the binary would create exactly this path.
DEFAULT_DB="${HOME:-/nonexistent}/.config/mailstack/accounts.db"
[ ! -e "$DEFAULT_DB" ] \
  || fail "the binary created its DEFAULT db at $DEFAULT_DB — the MAILSTACK_DB override is not being honoured"
# Synthetic mail has its own default path and its own override. It is opened
# lazily, so this also fails if that laziness is ever lost and the file starts
# being created at boot on every host.
DEFAULT_LOCAL_DB="${HOME:-/nonexistent}/.config/mailstack/local-mail.db"
[ ! -e "$DEFAULT_LOCAL_DB" ] \
  || fail "the binary created its DEFAULT synthetic-mail db at $DEFAULT_LOCAL_DB — MAILSTACK_LOCAL_DB is not being honoured, or the store is no longer opened lazily"
echo "    wrote only $DB_PATH; default paths untouched"

step "SUCCESS — mailstack boots and answers"
echo "    server log: $LOG"
