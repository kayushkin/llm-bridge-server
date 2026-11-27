#!/usr/bin/env bash
# Boot-and-answer smoke test for tool-store.
#
# A repo can compile green and still ship a DEAD binary: Go 1.22+
# http.ServeMux panics on conflicting route patterns at *registration* time,
# which no compiler catches. tool-store registers 14 patterns (several of them
# overlapping, e.g. GET /tools/{id} vs GET /tools/by-name/{name}), so "it
# builds" proves very little. This script builds the binary from THIS
# checkout, boots it against a throwaway DB on a throwaway port, and drives
# every route group through real HTTP, asserting on parsed response bodies.
#
# Never touches live state:
#   * temp data dir  (never ~/.config/tool-store)
#   * temp port      (never :8302)
#   * HOME is redirected into the temp dir too, so even a bug that ignored
#     TOOL_STORE_DATA_DIR could not reach the real DB.
#
# No external network and no live auth-store: AUTH_STORE_URL points at an
# unreachable address on purpose. Boot never contacts auth-store (the resolver
# in cmd/tool-store/authstore.go is a lazily-invoked closure), and the only
# route that would call it is POST /provision for a tool with env_keys. We
# exercise both sides of that: provisioning a credential-free MCP tool must
# SUCCEED, and provisioning a credential-bearing one must fail LOUDLY with a
# 400 naming auth-store — never hang, never silently emit an empty secret.
#
# Exits 0 on success, non-zero on the first failing assertion; the server log
# is dumped to stderr on failure.
#
# Tunables:
#   E2E_PORT   — listen port (default 19109)
#   E2E_KEEP   — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19109}"
BASE="http://127.0.0.1:$PORT"

# Unreachable on purpose. Port 1 (tcpmux) is never bound here, so connects fail
# with an instant ECONNREFUSED rather than a timeout — the failure path stays
# fast and deterministic.
UNREACHABLE_AUTH_STORE="http://127.0.0.1:1"

for bin in go curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t tool-store-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
FAKE_HOME="$TMP_DIR/home"
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
#   assertions that care about the status (including the 4xx/409 negatives), so
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
# Also note `jq -r`, not `jq -e`: with -e, a filter that legitimately yields
# `false` (e.g. `.enabled` on a disabled tool) sets exit status 1, which would
# be indistinguishable from a broken filter.
jq_eq() {
  local filter="$1" want="$2" ctx="$3" got
  got=$(jq -r "$filter" <"$RESP" 2>&1) \
    || fail "$ctx: jq '$filter' errored: $got — body: $(cat "$RESP")"
  [ "$got" = "$want" ] \
    || fail "$ctx: '$filter' expected [$want], got [$got] — body: $(cat "$RESP")"
}

# jq_val FILTER — extract a value from $RESP. Only for values the caller then
# validates itself (ids). A hard jq error fails the assignment, which `set -e`
# turns into an abort.
jq_val() { jq -r "$1" <"$RESP"; }

# jq_true FILTER CONTEXT — assert a boolean jq predicate holds. Safe to run
# `fail` from here: this is a statement, not a substitution.
jq_true() {
  jq -e "$1" <"$RESP" >/dev/null 2>&1 \
    || fail "$2 — predicate '$1' did not hold on body: $(cat "$RESP")"
}

step "build tool-store from $REPO_DIR"
cd "$REPO_DIR"
go build -o "$BIN_DIR/tool-store" ./cmd/tool-store
echo "    binary: $(ls -lh "$BIN_DIR/tool-store" | awk '{print $5}')"

step "launch tool-store on :$PORT (data=$DATA_DIR, auth-store=$UNREACHABLE_AUTH_STORE)"
# TOOL_STORE_DATA_DIR keeps the SQLite file in the temp dir; HOME is redirected
# as a second line of defense (toolstore.DefaultDataDir() is $HOME/.config/…).
# AUTH_STORE_TOKEN is deliberately empty — we never talk to a real auth-store.
env -u AUTH_STORE_TOKEN \
  HOME="$FAKE_HOME" \
  TOOL_STORE_ADDR=":$PORT" \
  TOOL_STORE_DATA_DIR="$DATA_DIR" \
  AUTH_STORE_URL="$UNREACHABLE_AUTH_STORE" \
  "$BIN_DIR/tool-store" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

# Poll /health — a route-pattern panic dies during RegisterHandlers, i.e.
# before the listener ever opens, so a boot panic surfaces here as "never
# became ready" plus the panic trace in the dumped log.
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
[ -f "$DATA_DIR/tool-store.db" ] || fail "expected a fresh SQLite DB at $DATA_DIR/tool-store.db"
[ ! -e "$FAKE_HOME/.config/tool-store" ] \
  || fail "server fell back to the default \$HOME data dir despite TOOL_STORE_DATA_DIR"
echo "    db: $DATA_DIR/tool-store.db"

# The settings the binary serves are the ones it runs on: the port this script
# chose, read from the environment. A registry that is built and never read from
# would show the default here, and no unit test sees that.
step "GET /settings — the listen address is the one this script set"
STATUS=$(api GET /settings)
expect_status 200 "$STATUS" "GET /settings"
jq_eq '.service' 'tool-store' "GET /settings"
jq_true "any(.settings[]; .key == \"listen_address\" and .value == \":$PORT\" and .source == \"environment\")" \
  "GET /settings does not show listen_address=:$PORT from the environment"
jq_true "any(.settings[]; .key == \"data_directory\" and .value == \"$DATA_DIR\")" \
  "GET /settings does not show data_directory=$DATA_DIR"
echo "    settings OK"

# ---------------------------------------------------------------------------
# Seeds. First boot against an empty file must run the schema migration, seed
# the in-process local tools (cmd/tool-store/main.go seedLocalTools), the
# curated MCP servers (seeds.go seedMCPTools) and the Claude Code and Codex
# built-in tools (harness_seeds.go seedHarnessTools), then serve them back. Asserting
# these round-trip proves migrate + seed + query all work on a virgin DB.
# ---------------------------------------------------------------------------
step "GET /locals — in-process tool registry is exposed"
STATUS=$(api GET /locals)
expect_status 200 "$STATUS" "GET /locals"
LOCALS_COUNT=$(jq_val 'length')
[ "$LOCALS_COUNT" -ge 1 ] || fail "GET /locals returned no in-process tools"
for want in end_turn shell_commands ripgrep web_fetch scheduler browser web_search; do
  jq_true "any(.[]; .name == \"$want\")" "GET /locals missing expected in-process tool '$want'"
done
# Every descriptor must carry a description and an input schema, or harnesses
# downstream get an unusable tool definition.
jq_true 'all(.[]; (.description | length > 0) and (.input_schema != null))' \
  "GET /locals: some descriptor has an empty description or null input_schema"
jq -r '.[].name' <"$RESP" | sort >"$TMP_DIR/locals.names"
echo "    $LOCALS_COUNT in-process tools: $(paste -sd' ' "$TMP_DIR/locals.names")"

step "GET /tools?kind=local — seeder persisted every in-process tool"
STATUS=$(api GET '/tools?kind=local')
expect_status 200 "$STATUS" "GET /tools?kind=local"
jq -r '.[].name' <"$RESP" | sort >"$TMP_DIR/seeded.names"
# Cross-check the persisted rows against the live registry rather than against a
# hardcoded count: this asserts seedLocalTools covered the registry exactly, and
# keeps passing when a tool is added or removed in tools/register.go.
if ! diff -u "$TMP_DIR/locals.names" "$TMP_DIR/seeded.names" >"$TMP_DIR/locals.diff"; then
  echo "----- /locals vs seeded kind=local -----" >&2
  cat "$TMP_DIR/locals.diff" >&2
  fail "seeded kind=local rows do not match the in-process registry"
fi
# Seeds must arrive disabled — operators opt each tool in explicitly.
jq_true 'all(.[]; .enabled == false)' "freshly seeded local tools should default to enabled=false"
jq_true 'all(.[]; .local.symbol == .name)' "seeded local tools should carry local.symbol == name"
echo "    $(jq_val 'length') local rows, all disabled, symbols intact"

step "GET /tools?kind=mcp — curated MCP servers seeded"
STATUS=$(api GET '/tools?kind=mcp')
expect_status 200 "$STATUS" "GET /tools?kind=mcp"
for want in brave-search playwright chrome-devtools \
            context7 deepwiki fetch time knowledge-graph-memory sequential-thinking \
            github sentry notion firecrawl exa supabase; do
  jq_true "any(.[]; .name == \"$want\")" "GET /tools?kind=mcp missing seeded server '$want'"
done
# Every curated MCP seed arrives disabled on a virgin DB, like the locals.
jq_true 'all(.[]; .enabled == false)' "freshly seeded mcp tools should default to enabled=false"
# deepwiki is the one http-transport seed — url must persist and command stay empty.
jq_true '.[] | select(.name=="deepwiki")
         | (.mcp.transport == "http") and (.mcp.url == "https://mcp.deepwiki.com/mcp") and ((.mcp.command // "") == "")' \
  "deepwiki seed lost its http transport or url in the DB round-trip"
# The launcher spec must survive the encode → SQLite → decode round-trip.
jq_eq '.[] | select(.name=="playwright") | .mcp.command' 'npx' "playwright seed"
jq_true '.[] | select(.name=="playwright") | .mcp.args | index("--headless") != null' \
  "playwright mcp.args lost --headless in the DB round-trip"
# brave-search is the credential-bearing seed — env_keys/credentials must persist.
jq_true '.[] | select(.name=="brave-search")
         | (.env_keys | index("BRAVE_API_KEY") != null) and (.credentials.BRAVE_API_KEY == "brave")' \
  "brave-search seed lost its env_keys/credentials mapping"
echo "    mcp seeds OK (playwright launcher + brave-search credential mapping round-tripped)"

step "GET /kinds — the kind vocabulary, harness included"
STATUS=$(api GET /kinds)
expect_status 200 "$STATUS" "GET /kinds"
jq_eq '[.[].kind] | join(",")' 'mcp,cli,local,harness' "GET /kinds"
jq_true 'all(.[]; .description | length > 0)' "every kind on GET /kinds needs a description"

step "GET /tools?kind=harness&harness=… — harness tools seeded enabled"
STATUS=$(api GET '/tools?kind=harness&harness=claude_code')
expect_status 200 "$STATUS" "GET /tools?kind=harness&harness=claude_code"
jq_eq 'length' '37' "claude_code harness seed count"
jq_true 'all(.[]; .enabled == true and .kind == "harness" and .harness == "claude_code" and .name == ("claude_code." + .harness_tool_name))' \
  "claude_code harness rows should be enabled and named claude_code.<harness_tool_name>"
jq_eq '[.[] | select(.tags | index("runs-commands")) | .name] | join(",")' 'claude_code.Bash,claude_code.Monitor' \
  "claude_code runs-commands tags"
STATUS=$(api GET '/tools?kind=harness&harness=codex')
expect_status 200 "$STATUS" "GET /tools?kind=harness&harness=codex"
jq_eq 'length' '12' "codex harness seed count"
jq_eq '[.[] | select(.tags | index("runs-commands")) | .name] | join(",")' 'codex.shell_tool,codex.unified_exec' \
  "codex runs-commands tags"
STATUS=$(api GET '/tools?kind=harness&harness=no-such-harness')
expect_status 200 "$STATUS" "GET /tools?harness=no-such-harness"
jq_eq 'length' '0' "an unknown harness id matches nothing"
STATUS=$(api POST '/tools/by-name/claude_code.Bash/invoke' '{"command":"true"}')
expect_status 409 "$STATUS" "invoke on a harness tool"
jq_true '.error | test("harness")' "invoke on a harness tool should say the harness runs it"
STATUS=$(api GET '/tools/by-name/codex.shell_tool/spec')
expect_status 409 "$STATUS" "spec of a harness tool"
STATUS=$(api POST /provision '{"tools":["claude_code.Read"]}')
expect_status 400 "$STATUS" "POST /provision with a harness tool"
echo "    harness seeds OK"

# ---------------------------------------------------------------------------
# Write path: register a tool through the real route, read it back, run it.
# ---------------------------------------------------------------------------
TOOL_NAME="e2e-smoke-echo"
MARKER="hello-from-e2e-$$"

step "POST /tools — register a kind=cli tool"
STATUS=$(api POST /tools "$(cat <<JSON
{
  "name": "$TOOL_NAME",
  "display_name": "E2E Smoke Echo",
  "description": "Echoes its input. Registered by scripts/e2e-smoke.sh.",
  "kind": "cli",
  "tags": ["e2e", "smoke"],
  "cli": {
    "command": "echo",
    "args_template": ["{{text}}"],
    "timeout_ms": 5000
  },
  "enabled": true
}
JSON
)")
expect_status 201 "$STATUS" "POST /tools (insert)"
TOOL_ID=$(jq_val '.id')
[ "$TOOL_ID" -gt 0 ] 2>/dev/null || fail "POST /tools returned no usable id: $(cat "$RESP")"
echo "    id: $TOOL_ID"

step "GET /tools/by-name/$TOOL_NAME — read back what we wrote"
STATUS=$(api GET "/tools/by-name/$TOOL_NAME")
expect_status 200 "$STATUS" "GET /tools/by-name/$TOOL_NAME"
CTX="GET /tools/by-name/$TOOL_NAME"
jq_eq '.id'                   "$TOOL_ID"         "$CTX"
jq_eq '.kind'                 'cli'              "$CTX"
jq_eq '.cli.command'          'echo'             "$CTX"
jq_eq '.cli.args_template[0]' '{{text}}'         "$CTX"
jq_eq '.cli.timeout_ms'       '5000'             "$CTX"
jq_eq '.display_name'         'E2E Smoke Echo'   "$CTX"
jq_eq '.enabled'              'true'             "$CTX"
jq_eq '.tags | sort | join(",")' 'e2e,smoke'     "$CTX"
echo "    round-trip OK"

step "GET /tools/{id} — the numeric-id route resolves the same row"
STATUS=$(api GET "/tools/$TOOL_ID")
expect_status 200 "$STATUS" "GET /tools/$TOOL_ID"
jq_eq '.name' "$TOOL_NAME" "GET /tools/$TOOL_ID"

step "GET /tools filters — ?q= and ?tag= and ?enabled=true find it"
STATUS=$(api GET "/tools?q=$TOOL_NAME")
expect_status 200 "$STATUS" "GET /tools?q="
jq_true "any(.[]; .name == \"$TOOL_NAME\")" "?q=$TOOL_NAME did not match the tool we just registered"
STATUS=$(api GET '/tools?tag=e2e')
expect_status 200 "$STATUS" "GET /tools?tag=e2e"
jq_true "any(.[]; .name == \"$TOOL_NAME\")" "?tag=e2e did not match the tool we just registered"
STATUS=$(api GET '/tools?enabled=true')
expect_status 200 "$STATUS" "GET /tools?enabled=true"
# Every local and mcp seed is disabled and every harness seed is enabled, so
# our tool must be the only enabled row that is not kind=harness on a fresh DB.
ENABLED_NAMES=$(jq -r '.[] | select(.kind != "harness") | .name' <"$RESP" | paste -sd' ')
[ "$ENABLED_NAMES" = "$TOOL_NAME" ] \
  || fail "?enabled=true should list exactly [$TOOL_NAME] besides harness tools on a fresh DB, got [$ENABLED_NAMES]"
STATUS=$(api GET '/tools?kind=bogus')
expect_status 400 "$STATUS" "GET /tools?kind=bogus (invalid kind must be rejected)"
echo "    filters OK"

step "GET /tools/by-name/$TOOL_NAME/spec — cli spec is served"
STATUS=$(api GET "/tools/by-name/$TOOL_NAME/spec")
expect_status 200 "$STATUS" "GET /tools/by-name/$TOOL_NAME/spec"
jq_eq '.command' 'echo' "GET /tools/by-name/$TOOL_NAME/spec"

step "POST /tools/by-name/$TOOL_NAME/invoke — the CLI runner actually runs it"
STATUS=$(api POST "/tools/by-name/$TOOL_NAME/invoke" "{\"text\":\"$MARKER\"}")
expect_status 200 "$STATUS" "POST invoke (cli)"
OUTPUT=$(jq_val '.output')
# Asserts placeholder substitution AND process exec AND stdout capture.
case "$OUTPUT" in
  *"$MARKER"*) ;;
  *) fail "cli invoke output did not contain '$MARKER': $OUTPUT" ;;
esac
echo "    output: $(printf '%s' "$OUTPUT" | tr -d '\n')"

step "POST /tools/by-name/$TOOL_NAME/invoke — a missing placeholder fails loudly"
STATUS=$(api POST "/tools/by-name/$TOOL_NAME/invoke" '{"wrong_field":"x"}')
expect_status 500 "$STATUS" "POST invoke with missing placeholder input"
jq_true '.error | test("missing input field")' \
  "missing-placeholder invoke should name the missing field"

step "POST /tools — same name again upserts in place (200, id preserved)"
STATUS=$(api POST /tools "$(cat <<JSON
{
  "name": "$TOOL_NAME",
  "display_name": "E2E Smoke Echo",
  "description": "Updated by scripts/e2e-smoke.sh.",
  "kind": "cli",
  "tags": ["e2e", "smoke"],
  "cli": { "command": "echo", "args_template": ["{{text}}"], "timeout_ms": 5000 },
  "enabled": true
}
JSON
)")
expect_status 200 "$STATUS" "POST /tools (update)"
jq_eq '.id' "$TOOL_ID" "POST /tools (upsert-by-name must not allocate a new id)"
STATUS=$(api GET "/tools/by-name/$TOOL_NAME")
expect_status 200 "$STATUS" "GET after upsert"
jq_eq '.description' 'Updated by scripts/e2e-smoke.sh.' "upsert did not persist the new description"
echo "    upsert OK (id $TOOL_ID preserved)"

step "POST /tools/$TOOL_ID/disable — a disabled tool refuses to run (409)"
STATUS=$(api POST "/tools/$TOOL_ID/disable")
expect_status 200 "$STATUS" "POST /tools/$TOOL_ID/disable"
jq_eq '.enabled' 'false' "POST /tools/$TOOL_ID/disable"
STATUS=$(api POST "/tools/by-name/$TOOL_NAME/invoke" "{\"text\":\"$MARKER\"}")
expect_status 409 "$STATUS" "invoke on a disabled tool"
STATUS=$(api POST "/tools/$TOOL_ID/enable")
expect_status 200 "$STATUS" "POST /tools/$TOOL_ID/enable"
jq_eq '.enabled' 'true' "POST /tools/$TOOL_ID/enable"
echo "    enable/disable gating OK"

# ---------------------------------------------------------------------------
# kind=local dispatch — proves HandlerOptions.InvokeLocal is wired to the
# in-process registry, not just that the row exists in SQLite. end_turn is the
# only local tool with no side effects (it returns a fixed string).
# ---------------------------------------------------------------------------
step "enable + invoke the local tool 'end_turn' (in-process dispatch)"
STATUS=$(api GET '/tools/by-name/end_turn')
expect_status 200 "$STATUS" "GET /tools/by-name/end_turn"
END_TURN_ID=$(jq_val '.id')
STATUS=$(api POST "/tools/$END_TURN_ID/enable")
expect_status 200 "$STATUS" "enable end_turn"
STATUS=$(api POST '/tools/by-name/end_turn/invoke' '{"reason":"e2e smoke"}')
expect_status 200 "$STATUS" "POST invoke (local)"
jq_eq '.output' 'turn ended' "local invoke of end_turn"
echo "    local dispatch OK"

# ---------------------------------------------------------------------------
# Per-instance opt-in.
# ---------------------------------------------------------------------------
INSTANCE_ID="e2e-smoke-instance"
step "instance opt-in: POST/GET/DELETE /instances/$INSTANCE_ID/tools"
STATUS=$(api POST "/instances/$INSTANCE_ID/tools/by-name/$TOOL_NAME")
expect_status 200 "$STATUS" "opt instance into $TOOL_NAME"
STATUS=$(api GET "/instances/$INSTANCE_ID/tools")
expect_status 200 "$STATUS" "GET /instances/$INSTANCE_ID/tools"
jq_true "any(.[]; .name == \"$TOOL_NAME\")" "instance tool list does not contain $TOOL_NAME"
STATUS=$(api GET "/tools/by-name/$TOOL_NAME/instances")
expect_status 200 "$STATUS" "GET /tools/by-name/$TOOL_NAME/instances"
jq_eq 'join(",")' "$INSTANCE_ID" "reverse instance lookup"
# Global disabled is master: a globally-disabled tool cannot be opted into.
STATUS=$(api POST "/instances/$INSTANCE_ID/tools/by-name/browser")
expect_status 409 "$STATUS" "opting an instance into a globally-disabled tool must 409"
STATUS=$(api DELETE "/instances/$INSTANCE_ID/tools/by-name/$TOOL_NAME")
expect_status 200 "$STATUS" "remove instance opt-in"
STATUS=$(api GET "/instances/$INSTANCE_ID/tools")
expect_status 200 "$STATUS" "GET /instances/$INSTANCE_ID/tools (after delete)"
jq_eq 'length' '0' "instance tool list should be empty after DELETE"
echo "    instance opt-in OK"

# ---------------------------------------------------------------------------
# /provision — the auth-store boundary, with auth-store unreachable.
# ---------------------------------------------------------------------------
step "POST /provision — a credential-free MCP tool provisions with auth-store DOWN"
STATUS=$(api GET '/tools/by-name/playwright')
expect_status 200 "$STATUS" "GET /tools/by-name/playwright"
PW_ID=$(jq_val '.id')
STATUS=$(api POST "/tools/$PW_ID/enable")
expect_status 200 "$STATUS" "enable playwright"
STATUS=$(api POST /provision '{"tools":["playwright"]}')
expect_status 200 "$STATUS" "POST /provision (no env keys)"
# The response must be a launchable Claude Code --mcp-config fragment.
jq_eq '.mcpServers.playwright.command' 'npx' "POST /provision"
jq_true '.mcpServers.playwright.args | index("--headless") != null' \
  "/provision lost --headless from the playwright args"
# No env keys → no auth-store call → no env block. If this ever grows a key,
# a credential leaked in from somewhere it shouldn't have.
jq_true '.mcpServers.playwright.env == null' \
  "/provision emitted an env block for a tool with no env_keys"
echo "    provisioned playwright with no auth-store contact"

step "POST /provision — a credential-bearing tool fails LOUDLY when auth-store is down"
STATUS=$(api GET '/tools/by-name/brave-search')
expect_status 200 "$STATUS" "GET /tools/by-name/brave-search"
BRAVE_ID=$(jq_val '.id')
STATUS=$(api POST "/tools/$BRAVE_ID/enable")
expect_status 200 "$STATUS" "enable brave-search"
STATUS=$(api POST /provision '{"tools":["brave-search"]}')
expect_status 400 "$STATUS" "POST /provision with an unreachable auth-store"
# Must name auth-store as the culprit, and must NOT quietly hand back a config
# carrying an empty secret.
jq_true '.error | test("auth-store")' "/provision failure should name auth-store"
jq_true '.mcpServers == null' \
  "/provision returned a half-configured mcpServers block on credential failure"
echo "    degraded correctly: $(jq_val '.error')"

step "POST /provision — argument validation"
STATUS=$(api POST /provision '{"tools":[]}')
expect_status 400 "$STATUS" "POST /provision with no tools"
STATUS=$(api POST /provision '{"tools":["no-such-tool"]}')
expect_status 400 "$STATUS" "POST /provision with an unknown tool"
STATUS=$(api POST /provision "{\"tools\":[\"$TOOL_NAME\"]}")
expect_status 400 "$STATUS" "POST /provision with a kind=cli tool (only mcp is provisionable)"

# ---------------------------------------------------------------------------
# Not-found and delete paths.
# ---------------------------------------------------------------------------
step "404 / 400 paths"
STATUS=$(api GET '/tools/by-name/definitely-not-a-tool')
expect_status 404 "$STATUS" "GET unknown tool by name"
STATUS=$(api GET '/tools/999999')
expect_status 404 "$STATUS" "GET unknown tool by id"
STATUS=$(api GET '/tools/not-a-number')
expect_status 400 "$STATUS" "GET /tools/{id} with a non-numeric id"
STATUS=$(api POST /tools '{"name":"bad","kind":"cli"}')
expect_status 400 "$STATUS" "POST /tools with a kind=cli body and no cli.command"

step "DELETE /tools/$TOOL_ID"
STATUS=$(api DELETE "/tools/$TOOL_ID")
expect_status 200 "$STATUS" "DELETE /tools/$TOOL_ID"
STATUS=$(api GET "/tools/by-name/$TOOL_NAME")
expect_status 404 "$STATUS" "GET after DELETE"
echo "    delete OK"

step "server is still alive and never logged a panic"
kill -0 "$SERVER_PID" 2>/dev/null || fail "server died during the run"
PANICS=$(grep -c -i -E 'panic:|fatal error:' "$SERVER_LOG" || true)
[ "$PANICS" = "0" ] || fail "server log contains $PANICS panic/fatal line(s)"

step "SUCCESS — tool-store boots, seeds, and answers"
echo "    server log: $SERVER_LOG"
