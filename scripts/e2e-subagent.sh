#!/usr/bin/env bash
# End-to-end check that a Claude Code Task subagent becomes its own bridge
# session, linked back to the session that spawned it.
#
# Claude Code runs Task subagents inside the parent's process and writes their
# frames onto the parent's stdout, all stamped with the parent's session_id.
# Only parent_tool_use_id separates them. This script drives a real subagent
# spawn and asserts the resulting lineage, because a unit test can only prove
# the demux works on frames we hand it — not that Claude Code still emits the
# field the demux keys on. If CC ever stops emitting parent_tool_use_id, every
# unit test here keeps passing and only this script fails.
#
# Skips cleanly when `claude` is missing, so it is safe to wire into CI.
#
# Tunables:
#   E2E_PORT           — listen port (default 18171)
#   E2E_LOG_STORE_PORT — log-store listen port (default 18177)
#   E2E_TIMEOUT        — seconds to wait for the round-trip (default 300)
#   E2E_KEEP           — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-18171}"
LOG_STORE_PORT="${E2E_LOG_STORE_PORT:-18177}"
TIMEOUT="${E2E_TIMEOUT:-300}"
BASE="http://127.0.0.1:$PORT"
LOG_STORE_BASE="http://127.0.0.1:$LOG_STORE_PORT"
LOG_STORE_REPO="$(dirname "$REPO_DIR")/log-store"
ADAPTER_REPO="$(dirname "$REPO_DIR")/llm-bridge-claudecode"

PROMPT="${E2E_PROMPT:-Spawn exactly one subagent with the Task tool (subagent_type Explore, description e2e-probe) and ask it to reply with the single word ping. Then reply with just that word.}"

for bin in go curl jq; do
  command -v "$bin" >/dev/null 2>&1 || { echo "ERROR: required tool '$bin' not found on PATH" >&2; exit 2; }
done
if ! command -v claude >/dev/null 2>&1; then
  echo "SKIP: claude CLI not on PATH"
  exit 0
fi
if [ ! -d "$ADAPTER_REPO" ]; then
  echo "SKIP: llm-bridge-claudecode sibling not found at $ADAPTER_REPO"
  exit 0
fi

TMP_DIR="$(mktemp -d -t llm-bridge-e2e-subagent.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
mkdir -p "$BIN_DIR" "$DATA_DIR"

SERVER_PID=""
LOG_STORE_PID=""
SSE_PID=""
cleanup() {
  for pid in "$SSE_PID" "$SERVER_PID" "$LOG_STORE_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [ "${E2E_KEEP:-}" = "1" ]; then
    echo "[e2e-subagent] keeping $TMP_DIR"
  else
    rm -rf "$TMP_DIR"
  fi
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; [ -f "$TMP_DIR/server.log" ] && tail -40 "$TMP_DIR/server.log" >&2; exit 1; }

step "build llm-bridge-server, log-store, and the claudecode adapter from source"
cd "$REPO_DIR"
go build -o "$BIN_DIR/llm-bridge-server" ./cmd/llm-bridge-server
[ -d "$LOG_STORE_REPO" ] || fail "log-store sibling not found at $LOG_STORE_REPO — run scripts/bootstrap.sh"
(cd "$LOG_STORE_REPO" && go build -o "$BIN_DIR/log-store" ./cmd/log-store)
# Build the adapter into the temp bin dir and put it FIRST on PATH. The demux
# needs the adapter's parent_tool_use_id passthrough, and a deployed adapter on
# the host may predate it — but overwriting the deployed binary would disturb
# live sessions, so shadow it for this run only.
(cd "$ADAPTER_REPO" && go build -o "$BIN_DIR/llm-bridge-claudecode" .)
export PATH="$BIN_DIR:$PATH"
echo "    adapter: $(command -v llm-bridge-claudecode)"

step "launch log-store on :$LOG_STORE_PORT"
LOG_STORE_LISTEN_ADDR=":$LOG_STORE_PORT" \
LOG_STORE_DB_PATH="$DATA_DIR/log-store.db" \
LOG_STORE_LOGSTACK_URL="http://127.0.0.1:1" \
  "$BIN_DIR/log-store" >"$TMP_DIR/log-store.log" 2>&1 &
LOG_STORE_PID=$!
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "$LOG_STORE_BASE/api/v1/sessions" 2>/dev/null && break
  sleep 0.1
done

step "launch server on :$PORT"
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
for _ in $(seq 1 50); do
  curl -fsS "$BASE/health" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "$BASE/health" >/dev/null 2>&1 || fail "server did not come up on $BASE within 10s"

step "register machine + claude_code instance"
MID=$(curl -fsS -X POST "$BASE/machines" -H 'Content-Type: application/json' \
  -d '{"name":"e2e-subagent-local","transport":"local"}' | jq -r '.id')
IID=$(curl -fsS -X POST "$BASE/instances" -H 'Content-Type: application/json' \
  -d "{\"name\":\"e2e-subagent\",\"harness_type\":\"claude_code\",\"machine_id\":\"$MID\"}" | jq -r '.id')
echo "    instance id: $IID"

step "create session and send a prompt that spawns a subagent"
# permission_mode=bypass short-circuits the prehook. Without it the prehook
# consults permission-store, which this run deliberately points at a dead port,
# and an unreachable store parks the call for a human who is not there — so the
# Agent tool never fires and the subagent under test is never spawned.
SID=$(curl -fsS -X POST "$BASE/sessions" -H 'Content-Type: application/json' \
  -d "{\"harness\":\"claude_code\",\"instance_id\":\"$IID\",\"auto_start\":false,\"source\":\"e2e-subagent\",\"harness_config\":{\"permission_mode\":\"bypass\"}}" \
  | jq -r '.session_id')
[ -n "$SID" ] && [ "$SID" != "null" ] || fail "POST /sessions returned no session id"
echo "    parent session: $SID"

EVENTS_FILE="$TMP_DIR/events.ndjson"
curl -sN --max-time "$TIMEOUT" "$BASE/sessions/$SID/events" >"$EVENTS_FILE" 2>&1 &
SSE_PID=$!
sleep 0.3

curl -fsS -X POST "$BASE/sessions/$SID/send" -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg m "$PROMPT" '{message:$m}')" >/dev/null

deadline=$(( $(date +%s) + TIMEOUT ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  grep -q '"type":"result"\|"type":"error"' "$EVENTS_FILE" 2>/dev/null && break
  sleep 1
done
kill "$SSE_PID" 2>/dev/null || true; wait "$SSE_PID" 2>/dev/null || true; SSE_PID=""

grep -q '"type":"result"' "$EVENTS_FILE" 2>/dev/null || fail "no result event within ${TIMEOUT}s"

step "assert the subagent became its own session, linked to its parent"
SESSIONS=$(curl -fsS "$BASE/sessions")
SUB=$(jq -c --arg p "$SID" '[.[] | select(.manager_session_id == $p)] | .[0] // empty' <<<"$SESSIONS")
if [ -z "$SUB" ]; then
  echo "----- sessions -----" >&2
  jq -r '.[] | "\(.session_id) purpose=\(.purpose) manager=\(.manager_session_id // "-")"' <<<"$SESSIONS" >&2
  echo "----- task narration seen on the parent -----" >&2
  grep -o '"subtype":"task_[a-z_]*"' "$EVENTS_FILE" | sort | uniq -c >&2
  fail "no session was linked to parent $SID via manager_session_id — the subagent did not get promoted"
fi

SUB_ID=$(jq -r '.session_id' <<<"$SUB")
echo "    subagent session: $SUB_ID"
jq -r '"    purpose=\(.purpose) type=\(.type) depth=\(.depth) controlled_by=\(.controlled_by) harness_session_id=\(.harness_session_id)"' <<<"$SUB"

check() { # field expected actual
  [ "$3" = "$2" ] || fail "$1 = $3, want $2"
}
check purpose        subagent       "$(jq -r '.purpose' <<<"$SUB")"
check type           system         "$(jq -r '.type' <<<"$SUB")"
check depth          1              "$(jq -r '.depth' <<<"$SUB")"
check controlled_by  harness        "$(jq -r '.controlled_by' <<<"$SUB")"
check root_session_id "$SID"        "$(jq -r '.root_session_id' <<<"$SUB")"

# The dedupe key must match the rollout filename discovery derives, or the
# scanner will mint a second, unlinked row for the same subagent later.
HSID=$(jq -r '.harness_session_id' <<<"$SUB")
case "$HSID" in
  agent-*) ;;
  *) fail "harness_session_id = $HSID, want agent-<task_id> to match subagents/agent-<task_id>.jsonl" ;;
esac

step "assert the subagent's own work landed in the subagent's session"
# /events is an SSE stream that stays open, so this must be time-bounded — an
# unbounded curl here hangs until the script's own timeout. It also replays
# stored events ONLY when given a Last-Event-ID; a bare connection streams live
# events, and the subagent has already finished by now, so without the header
# this reads an empty session and reports a bug that isn't there.
SUB_EVENTS_FILE="$TMP_DIR/sub-events.ndjson"
curl -sN --max-time 10 -H 'Last-Event-ID: 1' "$BASE/sessions/$SUB_ID/events" >"$SUB_EVENTS_FILE" 2>/dev/null || true
SUB_COUNT=$(grep -c '^data: ' "$SUB_EVENTS_FILE" || true)
echo "    events stored on the subagent session: $SUB_COUNT"
[ "${SUB_COUNT:-0}" -gt 0 ] || fail "subagent session holds no events — it was promoted but frames were not routed to it"

# Under the bridge's flags the subagent contributes exactly one frame: its
# prompt. Assert on that specifically, so this notices if CC's frame budget
# changes rather than passing on any event at all.
grep -q '"type":"user_message"' "$SUB_EVENTS_FILE" \
  || fail "subagent session holds no user_message — its prompt frame was dropped in translation"

step "assert the subagent settled rather than hanging at running"
SUB_STATE=$(curl -fsS "$BASE/sessions/$SUB_ID" | jq -r '.state // .session.state')
echo "    subagent state: $SUB_STATE"
case "$SUB_STATE" in
  idle|completed|error|aborted) ;;
  *) fail "subagent left in state '$SUB_STATE' — nothing else will ever settle it" ;;
esac

curl -fsS -X POST "$BASE/sessions/$SID/stop" >/dev/null || true

step "SUCCESS — subagent $SUB_ID linked to parent $SID"
