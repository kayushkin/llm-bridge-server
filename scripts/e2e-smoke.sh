#!/usr/bin/env bash
# End-to-end deploy smoke test using the mock harness.
#
# Builds llm-bridge-server and cmd/mock-harness from source, stages the
# mock binary on a temporary PATH as `llm-bridge-mock`, launches the
# server against a temp directory, drives a real session through the
# HTTP/SSE API, and asserts the expected events flow back. No LLM
# credentials or external services required.
#
# Exits 0 on success, non-zero on the first failing assertion. The
# server's full log is left at $TMP_DIR/server.log for post-mortem.
#
# Tunables:
#   E2E_PORT       — listen port (default 18160)
#   E2E_KEEP       — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-18160}"
LOG_STORE_PORT="${E2E_LOG_STORE_PORT:-18175}"
BASE="http://127.0.0.1:$PORT"
LOG_STORE_BASE="http://127.0.0.1:$LOG_STORE_PORT"
LOG_STORE_REPO="$(dirname "$REPO_DIR")/log-store"

for bin in go curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t llm-bridge-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
SANDBOX_HOME="$TMP_DIR/home"
mkdir -p "$BIN_DIR" "$DATA_DIR" "$SANDBOX_HOME"

# The PATH we hand the *server* — our freshly built mock and the system dirs it
# needs for git/ssh/sh, and nothing else. Deliberately excludes ~/bin.
#
# At boot the server runs auto-discovery, which execs every discoverable harness
# wrapper with -discover. Wrappers resolve through exec.LookPath (see
# planConformanceRun in internal/server/conformance.go: "PATH alone decides what
# a run covers"), so with the ambient PATH that reaches the host's real
# ~/bin/llm-bridge-claudecode and ~/bin/llm-bridge-codex — each of which opens
# its OWN state DB. Those are the LIVE session DBs that every running Claude
# Code session writes to. Curating PATH is what keeps this run off them, and it
# also makes the run reproducible: what the smoke exercises no longer depends on
# which harness wrappers happen to be installed on the host.
SERVER_PATH="$BIN_DIR:/usr/bin:/bin"

SERVER_PID=""
LOG_STORE_PID=""
cleanup() {
  for pid in "$SERVER_PID" "$LOG_STORE_PID"; do
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
fail() { echo "FAIL: $*" >&2; exit 1; }

step "build binaries"
cd "$REPO_DIR"
go build -o "$BIN_DIR/llm-bridge-server" ./cmd/llm-bridge-server
go build -o "$BIN_DIR/llm-bridge-mock"   ./cmd/mock-harness
echo "    server: $(ls -lh "$BIN_DIR/llm-bridge-server" | awk '{print $5}')"
echo "    mock:   $(ls -lh "$BIN_DIR/llm-bridge-mock"   | awk '{print $5}')"

# log-store is the durable event log + materialized message history. The
# server's /send handler refuses to forward a user_message that it cannot
# persist there, so the smoke run needs a real instance. Built from the
# sibling repo and run with a bogus logstack URL — logstack forwarding is
# fire-and-forget, so an unreachable target is logged-and-ignored.
if [ ! -d "$LOG_STORE_REPO" ]; then
  fail "log-store sibling not found at $LOG_STORE_REPO — run scripts/bootstrap.sh"
fi
(cd "$LOG_STORE_REPO" && go build -o "$BIN_DIR/log-store" ./cmd/log-store)
echo "    log-store: $(ls -lh "$BIN_DIR/log-store" | awk '{print $5}')"

step "launch log-store on :$LOG_STORE_PORT"
HOME="$SANDBOX_HOME" \
LOG_STORE_LISTEN_ADDR=":$LOG_STORE_PORT" \
LOG_STORE_DB_PATH="$DATA_DIR/log-store.db" \
LOG_STORE_LOGSTACK_URL="http://127.0.0.1:1" \
  "$BIN_DIR/log-store" >"$TMP_DIR/log-store.log" 2>&1 &
LOG_STORE_PID=$!
echo "    pid: $LOG_STORE_PID"
for _ in $(seq 1 50); do
  # log-store has no /health, but / responds even on errors; tcp open is enough.
  if curl -fsS -o /dev/null "$LOG_STORE_BASE/api/v1/sessions" 2>/dev/null; then break; fi
  sleep 0.1
done

step "launch server on :$PORT (data dir: $DATA_DIR)"
# PATH is $SERVER_PATH so exec.LookPath("llm-bridge-mock") resolves to our
# freshly built binary and no host-installed harness wrapper resolves at all.
#
# Every store below is pointed at the temp dir. HOME is pointed there too, and
# that is not redundant: internal/config defaults EVERY store path to somewhere
# under $HOME, so a store added later without a matching override here would
# silently open the LIVE database rather than an empty one. With HOME sandboxed
# the worst that mistake can cost is a stray temp file.
HOME="$SANDBOX_HOME" \
XDG_CONFIG_HOME="$SANDBOX_HOME/.config" \
XDG_DATA_HOME="$SANDBOX_HOME/.local/share" \
XDG_STATE_HOME="$SANDBOX_HOME/.local/state" \
LLMBRIDGE_LISTEN_ADDR=":$PORT" \
LLMBRIDGE_DB_PATH="$DATA_DIR/bridge.db" \
LLMBRIDGE_AGENT_DB="$DATA_DIR/agents.db" \
LLMBRIDGE_MEMORY_DB="$DATA_DIR/memory.db" \
LLMBRIDGE_HARNESS_DB="$DATA_DIR/harness.db" \
LLMBRIDGE_HOOK_DB="$DATA_DIR/hooks.db" \
LLMBRIDGE_MODEL_STORE_DB="$DATA_DIR/models.db" \
LLMBRIDGE_SNAPSHOT_DB="$DATA_DIR/snapshots.db" \
LLMBRIDGE_SNAPSHOT_GIT="$DATA_DIR/snapshots.git" \
LLMBRIDGE_BRIDGE_PREFS="$DATA_DIR/bridge-prefs.json" \
LLMBRIDGE_CONFORMANCE_PATH="$DATA_DIR/conformance.json" \
LLMBRIDGE_LOG_STORE_URL="$LOG_STORE_BASE" \
LLMBRIDGE_TOOL_STORE_URL="http://127.0.0.1:1" \
LLMBRIDGE_PERMISSION_STORE_URL="http://127.0.0.1:1" \
PATH="$SERVER_PATH" \
  "$BIN_DIR/llm-bridge-server" >"$TMP_DIR/server.log" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

# Poll /health until the listener accepts a connection (or give up after ~10s).
for _ in $(seq 1 50); do
  if curl -fsS "$BASE/health" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
if ! curl -fsS "$BASE/health" >/dev/null 2>&1; then
  echo "----- server.log -----" >&2
  cat "$TMP_DIR/server.log" >&2
  fail "server did not come up on $BASE within 10s"
fi
echo "    health OK"

step "verify /harnesses lists mock — and ONLY mock — as available"
HARNESSES=$(curl -fsS "$BASE/harnesses")
H=$(jq -r '.[] | select(.name=="mock") | "\(.name) available=\(.available)"' <<<"$HARNESSES")
[ -n "$H" ] || fail "/harnesses did not include mock"
echo "    $H"
echo "$H" | grep -q 'available=true' || fail "mock harness not marked available (binary not on PATH?)"

# The available set is exactly the set of wrappers the server can exec, so it is
# also the set auto-discovery will run at boot. Anything but "mock" here means
# the run has reached off its curated PATH and onto a host-installed wrapper —
# which would open that harness's live state DB. Assert the mechanism, not just
# the symptom: this fails the moment someone restores the ambient PATH.
AVAILABLE=$(jq -r '[.[] | select(.available) | .name] | sort | join(",")' <<<"$HARNESSES")
echo "    available harnesses: $AVAILABLE"
[ "$AVAILABLE" = "mock" ] \
  || fail "expected mock to be the only available harness, got: $AVAILABLE — the server's PATH is reaching host-installed wrappers, and boot auto-discovery will exec them against their LIVE state DBs"

step "POST /machines + /instances (local transport, harness=mock)"
# Sessions in llm-bridge must be bound to a harness-store instance — there
# is no local-spawn fallback. Mint a machine + instance once per smoke run.
MACHINE=$(curl -fsS -X POST "$BASE/machines" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-local","transport":"local"}')
MID=$(jq -r '.id' <<<"$MACHINE")
[ -n "$MID" ] && [ "$MID" != "null" ] || fail "POST /machines did not return id: $MACHINE"
echo "    machine id:  $MID"

INSTANCE=$(curl -fsS -X POST "$BASE/instances" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"e2e-mock\",\"harness_type\":\"mock\",\"machine_id\":\"$MID\"}")
IID=$(jq -r '.id' <<<"$INSTANCE")
[ -n "$IID" ] && [ "$IID" != "null" ] || fail "POST /instances did not return id: $INSTANCE"
echo "    instance id: $IID"

step "POST /sessions { harness:mock, instance_id:$IID, auto_start:false }"
# auto_start:false so the SSE subscriber can connect BEFORE the harness
# starts emitting events. SSE only replays current-turn events on connect,
# so subscribing after a finished turn yields a stale snapshot.
CREATE=$(curl -fsS -X POST "$BASE/sessions" \
  -H 'Content-Type: application/json' \
  -d "{\"harness\":\"mock\",\"instance_id\":\"$IID\",\"auto_start\":false,\"source\":\"e2e-smoke\"}")
SID=$(jq -r '.session_id' <<<"$CREATE")
[ -n "$SID" ] && [ "$SID" != "null" ] || fail "POST /sessions returned no session id: $CREATE"
echo "    session id: $SID"

# extract_types <file> → space-padded list of unique event types in the
# stream, e.g. " session_state session_info result ". Padding makes
# whole-token matches safe with grep -q " name ".
extract_types() {
  grep '^data: ' "$1" | sed 's/^data: //' \
    | jq -r '.type' 2>/dev/null | sort -u | paste -sd' '
}
extract_field() {
  grep '^data: ' "$1" | sed 's/^data: //' \
    | jq -r "select(.type==\"$2\") | $3" 2>/dev/null
}

step "subscribe to SSE then POST /sessions/$SID/send { message:'echo me' }"
EVENTS_FILE="$TMP_DIR/events1.ndjson"
curl -sN --max-time 5 "$BASE/sessions/$SID/events" >"$EVENTS_FILE" 2>&1 &
SSE_PID=$!
sleep 0.3  # let the subscriber complete the SSE handshake before we send

curl -fsS -X POST "$BASE/sessions/$SID/send" \
  -H 'Content-Type: application/json' \
  -d '{"message":"echo me"}' >/dev/null

wait "$SSE_PID" 2>/dev/null || true
TYPES=" $(extract_types "$EVENTS_FILE") "
echo "    types seen:$TYPES"
for want in session_state session_info user_message result; do
  echo "$TYPES" | grep -q " $want " || fail "expected $want event in stream"
done

USER_MSG=$(extract_field "$EVENTS_FILE" user_message '.result.text' | head -1)
RESULT_TEXT=$(extract_field "$EVENTS_FILE" result '.result.text' | tail -1)
echo "    user_message: $USER_MSG"
echo "    result text:  $RESULT_TEXT"
[ "$USER_MSG" = "echo me" ] || fail "user_message did not echo 'echo me'"
echo "$RESULT_TEXT" | grep -q "Mock response to: echo me" \
  || fail "result did not contain expected echo response"

step "POST /sessions/$SID/stop"
curl -fsS -X POST "$BASE/sessions/$SID/stop" >/dev/null

# After stop, listing should show the session in a terminal state
STATE=$(curl -fsS "$BASE/sessions/$SID" | jq -r '.state // .session.state')
echo "    state after stop: $STATE"
case "$STATE" in
  aborted|completed|disconnected|error|idle) ;;
  *) fail "unexpected post-stop state: $STATE (want aborted|completed|disconnected|error|idle)" ;;
esac

step "assert the run was hermetic (nothing written under HOME)"
# Both services ran with HOME inside TMP_DIR and every store path overridden, so
# a hermetic run leaves that HOME empty. Anything in it was written by a process
# that fell back to a $HOME default — with the real HOME that same write would
# have landed on a live database. Checked last, so boot auto-discovery has long
# since had its chance to run (it is a goroutine kicked off at startup).
# `|| true` because find's exit status is lost to SIGPIPE when head closes early,
# and under `set -o pipefail` that would abort the script instead of asserting.
STRAYS="$(find "$SANDBOX_HOME" -mindepth 1 2>/dev/null | head -10 || true)"
if [ -n "$STRAYS" ]; then
  echo "----- wrote under HOME -----" >&2
  echo "$STRAYS" >&2
  fail "run is not hermetic: the above paths were written under HOME. With the real HOME these are live databases (e.g. .local/share/llm-bridge-claudecode/state.db is the session DB every running Claude Code session writes to)."
fi
echo "    HOME sandbox clean: $SANDBOX_HOME"

step "SUCCESS"
echo "    server log:  $TMP_DIR/server.log"
echo "    events log:  $TMP_DIR/events1.ndjson  $TMP_DIR/events2.ndjson"
