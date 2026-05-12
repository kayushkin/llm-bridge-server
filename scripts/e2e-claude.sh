#!/usr/bin/env bash
# End-to-end deploy smoke test using the real claudecode harness.
#
# Same shape as e2e-smoke.sh, but drives a real LLM round-trip through
# the upstream `claude` CLI. Used to verify that a full deploy can talk
# to a real provider — not just the mock protocol.
#
# Skips cleanly (exit 0 with a clear message) if either `claude` or
# `llm-bridge-claudecode` is missing from PATH, so this script is safe
# to wire into CI on hosts that don't have those installed.
#
# Tunables:
#   E2E_PORT           — listen port (default 18161)
#   E2E_LOG_STORE_PORT — log-store listen port (default 18176)
#   E2E_PROMPT         — prompt to send (default "Say hello in one word.")
#   E2E_KEEP           — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-18161}"
LOG_STORE_PORT="${E2E_LOG_STORE_PORT:-18176}"
PROMPT="${E2E_PROMPT:-Say hello in one word.}"
BASE="http://127.0.0.1:$PORT"
LOG_STORE_BASE="http://127.0.0.1:$LOG_STORE_PORT"
LOG_STORE_REPO="$(dirname "$REPO_DIR")/log-store"

for bin in go curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

# Skip gates — both binaries must be present for the round-trip to work.
if ! command -v claude >/dev/null 2>&1; then
  echo "SKIP: claude CLI not on PATH"
  exit 0
fi
if ! command -v llm-bridge-claudecode >/dev/null 2>&1; then
  echo "SKIP: llm-bridge-claudecode not on PATH (build & install it: cd ~/repos/llm-bridge-claudecode && ./deploy.sh)"
  exit 0
fi
echo "[e2e-claude] claude:                 $(command -v claude)"
echo "[e2e-claude] llm-bridge-claudecode:  $(command -v llm-bridge-claudecode)"

TMP_DIR="$(mktemp -d -t llm-bridge-e2e-claude.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
mkdir -p "$BIN_DIR" "$DATA_DIR"

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
    echo "[e2e-claude] keeping $TMP_DIR"
  else
    rm -rf "$TMP_DIR"
  fi
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

step "build llm-bridge-server + log-store"
cd "$REPO_DIR"
go build -o "$BIN_DIR/llm-bridge-server" ./cmd/llm-bridge-server
if [ ! -d "$LOG_STORE_REPO" ]; then
  fail "log-store sibling not found at $LOG_STORE_REPO — run scripts/bootstrap.sh"
fi
(cd "$LOG_STORE_REPO" && go build -o "$BIN_DIR/log-store" ./cmd/log-store)

step "launch log-store on :$LOG_STORE_PORT"
LOG_STORE_LISTEN_ADDR=":$LOG_STORE_PORT" \
LOG_STORE_DB_PATH="$DATA_DIR/log-store.db" \
LOG_STORE_LOGSTACK_URL="http://127.0.0.1:1" \
  "$BIN_DIR/log-store" >"$TMP_DIR/log-store.log" 2>&1 &
LOG_STORE_PID=$!
echo "    pid: $LOG_STORE_PID"
for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null "$LOG_STORE_BASE/api/v1/sessions" 2>/dev/null; then break; fi
  sleep 0.1
done

step "launch server on :$PORT (data dir: $DATA_DIR)"
# Keep the host PATH (so llm-bridge-claudecode + claude remain reachable)
# but isolate every store to the temp directory.
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
  "$BIN_DIR/llm-bridge-server" >"$TMP_DIR/server.log" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

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

step "verify /harnesses lists claude_code as available"
H=$(curl -fsS "$BASE/harnesses" | jq -r '.[] | select(.name=="claude_code") | "\(.name) available=\(.available)"')
[ -n "$H" ] || fail "/harnesses did not include claude_code"
echo "    $H"
echo "$H" | grep -q 'available=true' || fail "claude_code not marked available"

step "POST /machines + /instances (local, claude_code)"
MACHINE=$(curl -fsS -X POST "$BASE/machines" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-claude-local","transport":"local"}')
MID=$(jq -r '.id' <<<"$MACHINE")
echo "    machine id:  $MID"

INSTANCE=$(curl -fsS -X POST "$BASE/instances" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"e2e-claude\",\"harness_type\":\"claude_code\",\"machine_id\":\"$MID\"}")
IID=$(jq -r '.id' <<<"$INSTANCE")
echo "    instance id: $IID"

step "POST /sessions (auto_start:false) then subscribe to SSE then /send"
CREATE=$(curl -fsS -X POST "$BASE/sessions" \
  -H 'Content-Type: application/json' \
  -d "{\"harness\":\"claude_code\",\"instance_id\":\"$IID\",\"auto_start\":false,\"source\":\"e2e-claude\"}")
SID=$(jq -r '.session_id' <<<"$CREATE")
[ -n "$SID" ] && [ "$SID" != "null" ] || fail "POST /sessions returned no session id: $CREATE"
echo "    session id: $SID"

# Real LLM round-trip needs a wider window than mock (claude CLI takes
# a moment to come up + a real API call + assistant generation).
EVENTS_FILE="$TMP_DIR/events.ndjson"
curl -sN --max-time 60 "$BASE/sessions/$SID/events" >"$EVENTS_FILE" 2>&1 &
SSE_PID=$!
sleep 0.3

curl -fsS -X POST "$BASE/sessions/$SID/send" \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg m "$PROMPT" '{message:$m}')" >/dev/null

# Wait for a terminator (result|error) before tearing down the subscriber,
# rather than burning the full 60s.
deadline=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  if grep -q '"type":"result"\|"type":"error"' "$EVENTS_FILE" 2>/dev/null; then
    break
  fi
  sleep 0.5
done
kill "$SSE_PID" 2>/dev/null || true
wait "$SSE_PID" 2>/dev/null || true

TYPES=$(grep '^data: ' "$EVENTS_FILE" | sed 's/^data: //' \
  | jq -r '.type' 2>/dev/null | sort -u | paste -sd' ')
echo "    types seen: $TYPES"

# Real claudecode may not always emit user_message — claude_code's contract
# echoes prompts only when --include-partial-messages is set. We only require
# session_state and a terminating result (or error if the round-trip failed).
for want in session_state; do
  echo " $TYPES " | grep -q " $want " || fail "expected $want event in stream"
done
if echo " $TYPES " | grep -q ' error '; then
  ERR_MSG=$(grep '^data: ' "$EVENTS_FILE" | sed 's/^data: //' \
    | jq -r 'select(.type=="error") | .error.message' 2>/dev/null | head -1)
  fail "claude harness emitted error event: $ERR_MSG"
fi
echo " $TYPES " | grep -q ' result ' || fail "no result event received within 60s"

RESULT_TEXT=$(grep '^data: ' "$EVENTS_FILE" | sed 's/^data: //' \
  | jq -r 'select(.type=="result") | .result.text' 2>/dev/null | tail -1)
echo "    result text: $RESULT_TEXT"
[ -n "$RESULT_TEXT" ] && [ "$RESULT_TEXT" != "null" ] || fail "result text was empty"

step "POST /sessions/$SID/stop"
curl -fsS -X POST "$BASE/sessions/$SID/stop" >/dev/null
STATE=$(curl -fsS "$BASE/sessions/$SID" | jq -r '.state // .session.state')
echo "    state after stop: $STATE"
case "$STATE" in
  aborted|completed|disconnected|error|idle) ;;
  *) fail "unexpected post-stop state: $STATE" ;;
esac

step "SUCCESS"
echo "    server log:  $TMP_DIR/server.log"
echo "    events log:  $TMP_DIR/events.ndjson"
