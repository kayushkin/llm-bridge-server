#!/usr/bin/env bash
# Boot-and-answer smoke test for usage-store.
#
# Builds usage-store-server from this repo's source, seeds a temp token-usage
# DB through the library's own Track(), launches the server against temp DBs on
# a temp port, and drives every registered route over real HTTP — asserting the
# parsed response bodies contain what was written.
#
# Why this exists beyond `go build`: Go 1.22+ http.ServeMux panics on a
# conflicting route pattern at *registration* time, so a repo can compile green
# and still ship a binary that dies the instant it boots. Nothing but booting it
# proves otherwise.
#
# Hermetic by construction:
#   * temp port          — never the production listener
#   * temp limits DB     — never ~/.config/usage-store/usage.db
#   * temp tokens DB     — never ~/.config/model-store/store.db
#   * HOME=$TMP_DIR/home — so the anthropic collector finds no
#                          ~/.claude/.credentials.json and makes NO network call,
#                          and the codex reader finds no ~/.codex/sessions.
#   * auth-store pointed at an unreachable URL — main.go's buildSpendCollectors
#                          logs the failure and returns no collectors, and the
#                          service still serves everything else. That degradation
#                          is asserted below, not assumed.
#
# Exits 0 on success, non-zero on the first failing assertion. On failure the
# server log is dumped to stderr.
#
# Tunables:
#   E2E_PORT — listen port (default 19110)
#   E2E_KEEP — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19110}"
BASE="http://127.0.0.1:$PORT"

# Values written by the smoke and asserted back out of the HTTP responses.
SEED_AGENT="e2e-smoke-agent"
SEED_ORCH="e2e-smoke-orch"
SEED_MODEL="e2e-smoke-model"
SEED_SESSION="e2e-smoke-session"
SEED_HARNESS="e2e-smoke-harness"
SEED_INPUT=1234
SEED_OUTPUT=567
SEED_COST="0.4242"
SEED_COST_MILLI=424    # SEED_COST * 1000, rounded — float-safe comparison

TOPUP_AMOUNT="12.34"
TOPUP_AMOUNT_CENTS=1234
TOPUP_DATE="2026-01-01"
TOPUP_DATE_UNIX=1767225600   # 2026-01-01T00:00:00Z — handleAddTopup parses
                             # occurred_at_str as UTC midnight
TOPUP_NOTE="e2e-smoke top-up"

for bin in go curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

step() { printf '\n==> %s\n' "$*"; }
dump_log() {
  if [ -f "${TMP_DIR:-}/server.log" ]; then
    echo "----- server.log -----" >&2
    cat "$TMP_DIR/server.log" >&2
    echo "----------------------" >&2
  fi
}
fail() { echo "FAIL: $*" >&2; dump_log; exit 1; }

# Refuse to run against anything that is already listening on $PORT — that is
# almost certainly the live service, and this script must never touch it.
if curl -fsS --max-time 2 -o /dev/null "$BASE/health" 2>/dev/null; then
  echo "ERROR: something is already serving $BASE — refusing to run against a live service." >&2
  echo "       Set E2E_PORT to a free port." >&2
  exit 2
fi

TMP_DIR="$(mktemp -d -t usage-store-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
FAKE_HOME="$TMP_DIR/home"
mkdir -p "$BIN_DIR" "$DATA_DIR" "$FAKE_HOME"

LIMITS_DB="$DATA_DIR/usage.db"
TOKENS_DB="$DATA_DIR/tokens.db"

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

# GET/POST/DELETE returning the HTTP status on stdout, body written to $1.
# Deliberately not -f: several assertions are *about* the status code.
req() {
  local out="$1" method="$2" url="$3"; shift 3
  curl -s --max-time 10 -o "$out" -w '%{http_code}' -X "$method" "$url" "$@"
}

step "build usage-store-server from $REPO_DIR"
cd "$REPO_DIR"
go build -o "$BIN_DIR/usage-store-server" ./cmd/usage-store-server
echo "    binary: $(ls -lh "$BIN_DIR/usage-store-server" | awk '{print $5}')"

step "seed a token-usage row into the temp tokens DB"
# usage-store exposes NO HTTP write route for token-usage rows: /api/usage is a
# read-only aggregate over USAGE_STORE_TOKENS_DB, which model-store owns and
# writes in production via this same library call. So the seed goes through the
# repo's own usagestore.Open + Track — the real production write path — rather
# than a hand-copied CREATE TABLE that could silently drift from store.go.
#
# The file lives in $TMP_DIR (never in the repo, which must stay clean), but
# `go run` from $REPO_DIR compiles it against this module, so it links THIS
# source tree.
cat > "$TMP_DIR/seed-usage.go" <<'GOEOF'
package main

import (
	"fmt"
	"os"
	"strconv"

	usagestore "github.com/kayushkin/usage-store"
)

// usage: seed-usage <db> <agent> <orchestrator> <model> <session> <harness> <in> <out> <cost>
func main() {
	a := os.Args[1:]
	if len(a) != 9 {
		fmt.Fprintf(os.Stderr, "seed-usage: want 9 args, got %d\n", len(a))
		os.Exit(2)
	}
	in, err := strconv.ParseInt(a[6], 10, 64)
	must(err)
	out, err := strconv.ParseInt(a[7], 10, 64)
	must(err)
	cost, err := strconv.ParseFloat(a[8], 64)
	must(err)

	s, err := usagestore.Open(a[0])
	must(err)
	defer s.Close()

	// pricing=nil + non-zero costUSD → cost is taken verbatim, exactly how
	// claude-code-sourced billing rows are recorded.
	must(s.Track(a[1], a[2], a[3], in, out, cost, nil, a[4], a[5]))
	fmt.Printf("seeded %s in=%d out=%d cost=%v\n", a[1], in, out, cost)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed-usage:", err)
		os.Exit(1)
	}
}
GOEOF
go run "$TMP_DIR/seed-usage.go" \
  "$TOKENS_DB" "$SEED_AGENT" "$SEED_ORCH" "$SEED_MODEL" "$SEED_SESSION" "$SEED_HARNESS" \
  "$SEED_INPUT" "$SEED_OUTPUT" "$SEED_COST" \
  | sed 's/^/    /'
[ -s "$TOKENS_DB" ] || fail "seeder did not create $TOKENS_DB"

step "launch server on :$PORT (temp DBs, temp HOME, unreachable auth-store)"
USAGE_STORE_LISTEN_ADDR=":$PORT" \
USAGE_STORE_DB="$LIMITS_DB" \
USAGE_STORE_TOKENS_DB="$TOKENS_DB" \
USAGE_STORE_LIMITS_IDLE_REFRESH_INTERVAL="1h" \
USAGE_STORE_SPEND_REFRESH_INTERVAL="1h" \
AUTH_STORE_URL="http://127.0.0.1:1" \
AUTH_STORE_TOKEN="e2e-smoke-not-a-real-token" \
HOME="$FAKE_HOME" \
  "$BIN_DIR/usage-store-server" >"$TMP_DIR/server.log" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

# Poll /health until the listener answers. A ServeMux route conflict panics
# inside server.New() before ListenAndServe, so a dead binary shows up here as
# "never came up" with the panic in server.log.
READY=0
for _ in $(seq 1 60); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    fail "server process exited during startup"
  fi
  if curl -fsS --max-time 2 "$BASE/health" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.2
done
[ "$READY" = "1" ] || fail "server did not answer $BASE/health within ~12s"

BODY="$TMP_DIR/body.json"
CODE=$(req "$BODY" GET "$BASE/health")
[ "$CODE" = "200" ] || fail "GET /health → $CODE (want 200)"
[ "$(jq -r '.status' "$BODY")" = "ok" ] || fail "GET /health body not {status:ok}: $(cat "$BODY")"
echo "    health OK"

step "GET /api/usage — the seeded row must come back out of the aggregate"
CODE=$(req "$BODY" GET "$BASE/api/usage")
[ "$CODE" = "200" ] || fail "GET /api/usage → $CODE (want 200): $(cat "$BODY")"
for period in day week month; do
  ROW=$(jq -c --arg a "$SEED_AGENT" ".$period[]? | select(.agent==\$a)" "$BODY")
  [ -n "$ROW" ] || fail "GET /api/usage .$period has no row for agent $SEED_AGENT: $(cat "$BODY")"

  [ "$(jq -r '.orchestrator' <<<"$ROW")" = "$SEED_ORCH" ]   || fail ".$period orchestrator: $ROW"
  [ "$(jq -r '.model' <<<"$ROW")" = "$SEED_MODEL" ]         || fail ".$period model: $ROW"
  [ "$(jq -r '.input_tokens' <<<"$ROW")" = "$SEED_INPUT" ]  || fail ".$period input_tokens: $ROW"
  [ "$(jq -r '.output_tokens' <<<"$ROW")" = "$SEED_OUTPUT" ] || fail ".$period output_tokens: $ROW"
  [ "$(jq -r '.total_tokens' <<<"$ROW")" = "$((SEED_INPUT + SEED_OUTPUT))" ] || fail ".$period total_tokens: $ROW"
  [ "$(jq -r '.messages' <<<"$ROW")" = "1" ]                || fail ".$period messages (requests): $ROW"
  # Float-safe: compare millicents, not IEEE-754 equality.
  GOT_COST=$(jq -r '(.cost_usd * 1000) | round' <<<"$ROW")
  [ "$GOT_COST" = "$SEED_COST_MILLI" ] || fail ".$period cost_usd: want ~$SEED_COST, got $(jq -r '.cost_usd' <<<"$ROW")"
  echo "    $period: $ROW"
done

step "GET /api/usage/limits — both providers keyed, no snapshots (temp HOME)"
CODE=$(req "$BODY" GET "$BASE/api/usage/limits")
[ "$CODE" = "200" ] || fail "GET /api/usage/limits → $CODE (want 200): $(cat "$BODY")"
[ "$(jq -r 'has("anthropic") and has("codex")' "$BODY")" = "true" ] \
  || fail "GET /api/usage/limits missing provider keys: $(cat "$BODY")"
# No ~/.claude/.credentials.json and no ~/.codex/sessions under the temp HOME,
# so the startup refresher persisted nothing and both snapshots are null. This
# is the assertion that proves the run made no network call to Anthropic.
[ "$(jq -r '.anthropic' "$BODY")" = "null" ] \
  || fail "anthropic snapshot is non-null — the run reached the live Anthropic API: $(cat "$BODY")"
echo "    limits: $(cat "$BODY")"

step "POST /api/usage/limits/watch — the usage page marks itself open"
CODE=$(req "$BODY" POST "$BASE/api/usage/limits/watch")
[ "$CODE" = "204" ] || fail "POST /api/usage/limits/watch → $CODE (want 204): $(cat "$BODY")"
grep -q "usage page open" "$TMP_DIR/server.log" 2>/dev/null || { sleep 1; grep -q "usage page open" "$TMP_DIR/server.log"; } \
  || fail "watch did not switch the limits refresher to the watched interval: $(tail -5 "$TMP_DIR/server.log")"

step "GET /api/usage/limits/{provider} + /history — wildcard routes"
CODE=$(req "$BODY" GET "$BASE/api/usage/limits/anthropic")
[ "$CODE" = "404" ] || fail "GET /api/usage/limits/anthropic → $CODE (want 404 'no snapshot'): $(cat "$BODY")"

CODE=$(req "$BODY" GET "$BASE/api/usage/limits/anthropic/history?window=five_hour&limit=5")
[ "$CODE" = "200" ] || fail "GET .../history → $CODE (want 200): $(cat "$BODY")"
[ "$(jq -r 'type' "$BODY")" = "array" ] || fail "history did not return a JSON array: $(cat "$BODY")"
echo "    history rows: $(jq -r 'length' "$BODY")"

step "POST /api/usage/limits/refresh?provider=bogus — registered, rejects cleanly"
CODE=$(req "$BODY" POST "$BASE/api/usage/limits/refresh?provider=bogus")
[ "$CODE" = "400" ] || fail "POST /api/usage/limits/refresh?provider=bogus → $CODE (want 400): $(cat "$BODY")"

step "GET /api/usage/spend/keys — degrades gracefully with auth-store unreachable"
CODE=$(req "$BODY" GET "$BASE/api/usage/spend/keys")
[ "$CODE" = "200" ] || fail "GET /api/usage/spend/keys → $CODE (want 200): $(cat "$BODY")"
[ "$(jq -r '.anthropic.configured' "$BODY")" = "false" ] \
  || fail "anthropic.configured should be false when auth-store is unreachable: $(cat "$BODY")"
[ "$(jq -r '.anthropic.remaining_usd' "$BODY")" = "null" ] \
  || fail "remaining_usd should be null before any top-up: $(cat "$BODY")"
# Proof it *degraded* rather than never trying: main.go logs the failed
# auth-store sweep. `|| true` guards grep's exit-1-on-no-match under pipefail.
if ! grep -q 'auth-store list failed' "$TMP_DIR/server.log"; then
  fail "expected '[spend] auth-store list failed' in the log — the unreachable auth-store was never contacted"
fi
echo "    configured=false, auth-store failure logged and survived"

# With no admin collector, the refresh route must say so rather than 500 or hang.
CODE=$(req "$BODY" POST "$BASE/api/usage/spend/refresh?provider=anthropic")
[ "$CODE" = "404" ] || fail "POST /api/usage/spend/refresh?provider=anthropic → $CODE (want 404 'not configured'): $(cat "$BODY")"
CODE=$(req "$BODY" POST "$BASE/api/usage/spend/refresh?provider=bogus")
[ "$CODE" = "400" ] || fail "POST /api/usage/spend/refresh?provider=bogus → $CODE (want 400): $(cat "$BODY")"

step "POST /api/usage/spend/topups → read back → assert → DELETE"
CODE=$(req "$BODY" POST "$BASE/api/usage/spend/topups" \
  -H 'Content-Type: application/json' \
  -d "{\"provider\":\"anthropic\",\"amount_usd\":$TOPUP_AMOUNT,\"occurred_at_str\":\"$TOPUP_DATE\",\"note\":\"$TOPUP_NOTE\"}")
[ "$CODE" = "200" ] || fail "POST topups → $CODE (want 200): $(cat "$BODY")"
TOPUP_ID=$(jq -r '.id' "$BODY")
[ -n "$TOPUP_ID" ] && [ "$TOPUP_ID" != "null" ] || fail "POST topups returned no id: $(cat "$BODY")"
echo "    topup id: $TOPUP_ID"

CODE=$(req "$BODY" GET "$BASE/api/usage/spend/topups?provider=anthropic")
[ "$CODE" = "200" ] || fail "GET topups → $CODE (want 200): $(cat "$BODY")"
[ "$(jq -r 'length' "$BODY")" = "1" ] || fail "GET topups: want exactly 1 row, got $(cat "$BODY")"
ROW=$(jq -c '.[0]' "$BODY")
[ "$(jq -r '.id' <<<"$ROW")" = "$TOPUP_ID" ]            || fail "topup id round-trip: $ROW"
[ "$(jq -r '.provider' <<<"$ROW")" = "anthropic" ]      || fail "topup provider: $ROW"
[ "$(jq -r '.note' <<<"$ROW")" = "$TOPUP_NOTE" ]        || fail "topup note: $ROW"
[ "$(jq -r '.occurred_at' <<<"$ROW")" = "$TOPUP_DATE_UNIX" ] \
  || fail "topup occurred_at: want $TOPUP_DATE_UNIX (UTC midnight of $TOPUP_DATE), got $ROW"
[ "$(jq -r '(.amount_usd * 100) | round' <<<"$ROW")" = "$TOPUP_AMOUNT_CENTS" ] || fail "topup amount_usd: $ROW"
echo "    read back: $ROW"

# The top-up must now show up in the derived /spend/keys balance too — that is
# a second, independent read path over the same row (SumSpendSince + rollup).
CODE=$(req "$BODY" GET "$BASE/api/usage/spend/keys")
[ "$CODE" = "200" ] || fail "GET /api/usage/spend/keys (post-topup) → $CODE: $(cat "$BODY")"
[ "$(jq -r '(.anthropic.topups_total_usd * 100) | round' "$BODY")" = "$TOPUP_AMOUNT_CENTS" ] \
  || fail "spend/keys topups_total_usd did not reflect the top-up: $(cat "$BODY")"
[ "$(jq -r '(.anthropic.remaining_usd * 100) | round' "$BODY")" = "$TOPUP_AMOUNT_CENTS" ] \
  || fail "spend/keys remaining_usd should equal the top-up with zero spend: $(cat "$BODY")"
[ "$(jq -r '.anthropic.balance_since' "$BODY")" = "$TOPUP_DATE_UNIX" ] \
  || fail "spend/keys balance_since should be the top-up date: $(cat "$BODY")"
echo "    balance: remaining=\$$(jq -r '.anthropic.remaining_usd' "$BODY") since=$(jq -r '.anthropic.balance_since' "$BODY")"

CODE=$(req "$BODY" GET "$BASE/api/usage/spend/keys/anthropic/no-such-key/raw")
[ "$CODE" = "404" ] || fail "GET spend/keys/{provider}/{api_key_id}/raw → $CODE (want 404): $(cat "$BODY")"

CODE=$(req "$BODY" DELETE "$BASE/api/usage/spend/topups/$TOPUP_ID")
[ "$CODE" = "204" ] || fail "DELETE topups/$TOPUP_ID → $CODE (want 204): $(cat "$BODY")"
CODE=$(req "$BODY" GET "$BASE/api/usage/spend/topups?provider=anthropic")
[ "$(jq -r 'length' "$BODY")" = "0" ] || fail "topup survived DELETE: $(cat "$BODY")"
CODE=$(req "$BODY" DELETE "$BASE/api/usage/spend/topups/not-a-number")
[ "$CODE" = "400" ] || fail "DELETE topups/not-a-number → $CODE (want 400): $(cat "$BODY")"
echo "    deleted; list is empty again"

step "GET /settings — describes this process, and never serves the secret"
# The registry is what main read its configuration from, so the port this smoke
# chose must be the value served, from the environment. A registry that was
# built and never read from would show the default instead.
CODE=$(req "$BODY" GET "$BASE/settings")
[ "$CODE" = "200" ] || fail "GET /settings → $CODE (want 200): $(cat "$BODY")"
[ "$(jq -r '.service' "$BODY")" = "usage-store" ] || fail "GET /settings service: $(cat "$BODY")"
LISTEN=$(jq -c '.settings[] | select(.key=="listen_address")' "$BODY")
[ "$(jq -r '.value' <<<"$LISTEN")" = ":$PORT" ] && [ "$(jq -r '.source' <<<"$LISTEN")" = "environment" ] \
  || fail "GET /settings listen_address should be :$PORT from the environment: $LISTEN"
IDLE=$(jq -c '.settings[] | select(.key=="limits_idle_refresh_interval")' "$BODY")
[ "$(jq -r '.value' <<<"$IDLE")" = "1h" ] && [ "$(jq -r '.source' <<<"$IDLE")" = "environment" ] \
  || fail "GET /settings limits_idle_refresh_interval should be 1h from the environment: $IDLE"
WATCHED=$(jq -c '.settings[] | select(.key=="limits_watched_refresh_interval")' "$BODY")
[ "$(jq -r '.source' <<<"$WATCHED")" = "default" ] \
  || fail "GET /settings limits_watched_refresh_interval should be the default: $WATCHED"
if grep -q "e2e-smoke-not-a-real-token" "$BODY"; then
  fail "GET /settings served AUTH_STORE_TOKEN's value"
fi
CODE=$(req "$BODY" PUT "$BASE/settings/listen_address" -H 'Content-Type: application/json' -d '{"value":":1"}')
[ "$CODE" != "200" ] || fail "PUT /settings/listen_address → 200: a write route is mounted"
echo "    listen_address=:$PORT (environment), secret withheld, no write route"

step "server survived every route"
kill -0 "$SERVER_PID" 2>/dev/null || fail "server died during the run"

# Nothing outside $TMP_DIR may have been created.
[ -s "$LIMITS_DB" ] || fail "limits DB was never written at $LIMITS_DB — did the server use a different path?"

step "SUCCESS"
echo "    routes asserted: /health, /api/usage, /api/usage/limits,"
echo "                     /api/usage/limits/{provider}, /api/usage/limits/{provider}/history,"
echo "                     /api/usage/limits/refresh, /api/usage/limits/watch, /api/usage/spend/keys,"
echo "                     /api/usage/spend/keys/{provider}/{api_key_id}/raw,"
echo "                     /api/usage/spend/topups (GET/POST/DELETE), /settings"
echo "    server log: $TMP_DIR/server.log"
