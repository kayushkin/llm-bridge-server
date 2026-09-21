#!/usr/bin/env bash
# Boot-and-answer smoke test for the scheduler service.
#
# Builds cmd/scheduler from THIS repo's source, boots it against a throwaway
# SQLite DB and a throwaway NATS server, and drives the real HTTP API: create a
# job, list it, expand it onto the calendar, run it, hold it, disable it, delete
# it. Asserts on parsed response content, not just status codes.
#
# Why this exists: `go build` proves the tree compiles, not that the binary is
# alive. Go 1.22+ http.ServeMux panics on a conflicting route pattern at
# REGISTRATION time — the compiler never sees it, and neither does `go vet`. A
# repo can be green and still ship a binary that dies on startup. This test
# boots the thing and makes it answer.
#
# SAFETY — the live scheduler executes shell commands and spawns agents. This
# test is built so it cannot touch it:
#   * SCHEDULER_DB points at a fresh temp DB. The live DB (~/.scheduler/
#     scheduler.db) is never opened, not even read-only. HOME is also
#     redirected into the temp dir, so even the compiled-in default path
#     ($HOME/.scheduler/scheduler.db) cannot resolve to the real one.
#   * A fresh DB has zero jobs — nothing can fire. The first assertion below is
#     that GET /api/jobs returns an EMPTY list; if it ever returns jobs, we are
#     pointed at a real database and the run aborts immediately.
#   * NATS_URL points at a throwaway nats-server this script starts on a temp
#     port, never at the live bus on :4222. This matters: internal/bus
#     subscribes to `scheduler.commands` with a QUEUE GROUP, so a second
#     scheduler on the live bus would silently steal half of the live
#     scheduler's bus commands.
#   * The one job we create runs `echo <marker>` — it writes nothing, anywhere.
#     Its cron schedule ("0 0 1 1 *") cannot fire during the run; the only
#     execution is the explicit POST /run, and it is deleted before exit.
#   * Listens on ${E2E_PORT:-19104}, never the live 8092.
#
# WHY A REAL nats-server IS REQUIRED (and cannot be an unreachable stub):
#   cmd/scheduler/main.go only warns when its first NATS connect fails, but
#   bus.NewHandler then dials the same NATS_URL, returns an error, and main
#   log.Fatalf's on it. So with NATS_URL unreachable the process exits 1 before
#   it ever listens. Pointing it at the live bus is not an option (see above),
#   so the only safe way to boot it is to give it a private bus of its own.
#
# Exits 0 on success, non-zero on the FIRST failing assertion, dumping the
# server log to stderr.
#
# Tunables:
#   E2E_PORT           — scheduler listen port        (default 19104)
#   E2E_NATS_PORT      — throwaway NATS client port   (default 14222)
#   E2E_NATS_MON_PORT  — throwaway NATS monitor port  (default 18222)
#   E2E_NATS_BIN       — path to nats-server          (default: PATH, then ~/bin)
#   E2E_KEEP=1         — leave $TMP_DIR in place after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19104}"
NATS_PORT="${E2E_NATS_PORT:-14222}"
NATS_MON_PORT="${E2E_NATS_MON_PORT:-18222}"
BASE="http://127.0.0.1:$PORT"

for bin in go curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

# nats-server is a hard dependency (see header). Resolve it explicitly instead
# of trusting PATH: the nightly guard runs smokes with a minimal systemd PATH
# (/usr/bin:/bin and friends) AND with HOME redirected to a throwaway scratch
# directory — so neither `command -v nats-server` nor "$HOME/bin" finds the
# binary that is actually installed on this host. Ask passwd for the invoking
# user's real home as a last resort. That path is only ever READ (we exec a
# binary from it); nothing under it is opened or written.
NATS_BIN="${E2E_NATS_BIN:-}"
if [ -z "$NATS_BIN" ] && command -v nats-server >/dev/null 2>&1; then
  NATS_BIN="$(command -v nats-server)"
fi
if [ -z "$NATS_BIN" ]; then
  PASSWD_HOME="$(getent passwd "$(id -un)" 2>/dev/null | cut -d: -f6 || true)"
  CANDIDATES=("$HOME/bin/nats-server" /usr/local/bin/nats-server /usr/bin/nats-server)
  [ -n "${PASSWD_HOME:-}" ] && CANDIDATES+=("$PASSWD_HOME/bin/nats-server")
  for cand in "${CANDIDATES[@]}"; do
    if [ -x "$cand" ]; then NATS_BIN="$cand"; break; fi
  done
fi
if [ -z "$NATS_BIN" ] || [ ! -x "$NATS_BIN" ]; then
  echo "ERROR: nats-server not found (looked on PATH, \$HOME/bin, the passwd home," >&2
  echo "       /usr/local/bin, /usr/bin)." >&2
  echo "       The scheduler cannot boot without a reachable NATS (main.go fatals" >&2
  echo "       when bus.NewHandler cannot dial NATS_URL), and this test refuses to" >&2
  echo "       use the live bus. Set E2E_NATS_BIN=/path/to/nats-server." >&2
  exit 2
fi

TMP_DIR="$(mktemp -d -t scheduler-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
FAKE_HOME="$TMP_DIR/home"
DB_PATH="$DATA_DIR/scheduler.db"
SERVER_LOG="$TMP_DIR/server.log"
NATS_LOG="$TMP_DIR/nats.log"
mkdir -p "$BIN_DIR" "$DATA_DIR" "$FAKE_HOME"

SERVER_PID=""
NATS_PID=""
cleanup() {
  for pid in "$SERVER_PID" "$NATS_PID"; do
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
dump_logs() {
  echo "----- server.log -----" >&2
  cat "$SERVER_LOG" >&2 2>/dev/null || true
  echo "----- nats.log -----" >&2
  tail -20 "$NATS_LOG" >&2 2>/dev/null || true
}
fail() { echo "FAIL: $*" >&2; dump_logs; exit 1; }

# A unique marker, so a run's output can only have come from OUR job.
MARKER="e2e-smoke-$$-$(date +%s)"
JOB_NAME="e2e-smoke job $MARKER"
JOB_CMD="echo $MARKER"
# February 30th never happens, but robfig/cron accepts the expression, so use a
# yearly schedule instead: parseable, expandable onto a calendar, and it cannot
# fire inside a smoke run that lasts seconds.
JOB_SCHEDULE="0 0 1 1 *"

step "build cmd/scheduler from $REPO_DIR"
cd "$REPO_DIR"
go build -o "$BIN_DIR/scheduler" ./cmd/scheduler
echo "    scheduler: $(ls -lh "$BIN_DIR/scheduler" | awk '{print $5}')"

step "launch throwaway nats-server on 127.0.0.1:$NATS_PORT"
echo "    binary: $NATS_BIN"
# -m enables the monitoring endpoint, which gives us a real readiness probe
# (/healthz) instead of a blind sleep. Do NOT probe this with bash's /dev/tcp:
# tearing the descriptor down again needs a bare `exec`, and a bare `exec` with
# redirections applies them to the SCRIPT ITSELF — `exec 3>&- 2>/dev/null`
# silently sends this script's own stderr to /dev/null for the rest of the run,
# swallowing every FAIL message and log dump that follows it.
"$NATS_BIN" -a 127.0.0.1 -p "$NATS_PORT" -m "$NATS_MON_PORT" >"$NATS_LOG" 2>&1 &
NATS_PID=$!
nats_up=0
for _ in $(seq 1 50); do
  if ! kill -0 "$NATS_PID" 2>/dev/null; then
    fail "throwaway nats-server exited on startup (port $NATS_PORT/$NATS_MON_PORT already in use?)"
  fi
  if curl -fsS --max-time 2 "http://127.0.0.1:$NATS_MON_PORT/healthz" >/dev/null 2>&1; then
    nats_up=1
    break
  fi
  sleep 0.1
done
[ "$nats_up" = "1" ] || fail "throwaway nats-server not healthy on :$NATS_MON_PORT within 5s"
echo "    pid: $NATS_PID (healthy)"

step "launch scheduler on :$PORT (db: $DB_PATH)"
# HOME is redirected too, so the compiled-in default DB path cannot possibly
# resolve to the live ~/.scheduler/scheduler.db. SCHEDULER_TOKEN is forced empty
# so an ambient token in the environment cannot lock us out of our own server.
SCHEDULER_PORT="$PORT" \
SCHEDULER_DB="$DB_PATH" \
SCHEDULER_TOKEN="" \
NATS_URL="nats://127.0.0.1:$NATS_PORT" \
HOME="$FAKE_HOME" \
  "$BIN_DIR/scheduler" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

# Poll /health — never sleep-and-hope. A route-registration panic shows up here
# as a process that exited before it ever listened.
health=""
for _ in $(seq 1 50); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    fail "scheduler process exited during startup (route panic? bus fatal?)"
  fi
  if health=$(curl -fsS --max-time 2 "$BASE/health" 2>/dev/null); then break; fi
  health=""
  sleep 0.2
done
[ -n "$health" ] || fail "scheduler did not answer $BASE/health within 10s"
[ "$(jq -r '.status' <<<"$health")" = "ok" ] || fail "/health did not report status=ok: $health"
echo "    health OK: $health"

step "GET /api/jobs — must be EMPTY (proves we are on a fresh temp DB)"
JOBS=$(curl -fsS --max-time 10 "$BASE/api/jobs")
COUNT=$(jq 'length' <<<"$JOBS")
[ "$COUNT" = "0" ] || fail "fresh DB should hold 0 jobs but /api/jobs returned $COUNT — ABORTING, this may be a real database"
[ -f "$DB_PATH" ] || fail "SCHEDULER_DB was not honoured — no db at $DB_PATH"
echo "    0 jobs, db at $DB_PATH"

step "GET /settings — describes the values this process started with"
# A registry that is built and never read from would show the defaults (8092,
# ~/.scheduler/scheduler.db), which no unit test would notice.
SETTINGS=$(curl -fsS --max-time 10 "$BASE/settings")
setting() { jq -c --arg k "$1" '.settings[] | select(.key == $k)' <<<"$SETTINGS"; }
[ "$(jq -r '.service' <<<"$SETTINGS")" = "scheduler" ] || fail "/settings names another service: $SETTINGS"
[ "$(setting port | jq -r '.value + " " + .source')" = "$PORT environment" ] || fail "/settings port is not $PORT from the environment: $(setting port)"
[ "$(setting database_path | jq -r '.value + " " + .source')" = "$DB_PATH environment" ] || fail "/settings database_path is not $DB_PATH from the environment: $(setting database_path)"
[ "$(setting token | jq -r '.is_set')" = "false" ] || fail "/settings says a token is set: $(setting token)"
PUT_STATUS=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 -X PUT "$BASE/settings/port" -d '{"value":"1"}')
[ "$PUT_STATUS" != "200" ] || fail "PUT /settings/port was accepted; nothing is editable"
echo "    port $PORT and db $DB_PATH from the environment; PUT answered $PUT_STATUS"

step "POST /api/jobs — create harmless shell job"
CREATED=$(curl -fsS --max-time 10 -X POST "$BASE/api/jobs" \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg n "$JOB_NAME" --arg s "$JOB_SCHEDULE" --arg c "$JOB_CMD" \
        '{name:$n, description:"created by scripts/e2e-smoke.sh", schedule:$s, type:"shell", command:$c}')")
JOB_ID=$(jq -r '.id' <<<"$CREATED")
[ -n "$JOB_ID" ] && [ "$JOB_ID" != "null" ] || fail "POST /api/jobs returned no id: $CREATED"
[ "$(jq -r '.name'    <<<"$CREATED")" = "$JOB_NAME" ]     || fail "created job name mismatch: $CREATED"
[ "$(jq -r '.type'    <<<"$CREATED")" = "shell" ]         || fail "created job type should be shell: $CREATED"
[ "$(jq -r '.command' <<<"$CREATED")" = "$JOB_CMD" ]      || fail "created job command mismatch: $CREATED"
[ "$(jq -r '.enabled' <<<"$CREATED")" = "true" ]          || fail "created job should be enabled: $CREATED"
echo "    job id: $JOB_ID"

step "GET /api/jobs — our job comes back with the right fields"
JOBS=$(curl -fsS --max-time 10 "$BASE/api/jobs")
[ "$(jq 'length' <<<"$JOBS")" = "1" ] || fail "expected exactly 1 job after create, got: $JOBS"
MINE=$(jq -c --argjson id "$JOB_ID" '.[] | select(.id==$id)' <<<"$JOBS")
[ -n "$MINE" ] || fail "job $JOB_ID missing from /api/jobs: $JOBS"
[ "$(jq -r '.name'     <<<"$MINE")" = "$JOB_NAME" ]     || fail "listed job name mismatch: $MINE"
[ "$(jq -r '.schedule' <<<"$MINE")" = "$JOB_SCHEDULE" ] || fail "listed job schedule mismatch: $MINE"
[ "$(jq -r '.command'  <<<"$MINE")" = "$JOB_CMD" ]      || fail "listed job command mismatch: $MINE"
echo "    listed: $(jq -r '"\(.id) \(.name) [\(.schedule)]"' <<<"$MINE")"

step "GET /api/jobs/$JOB_ID — single-job route"
ONE=$(curl -fsS --max-time 10 "$BASE/api/jobs/$JOB_ID")
[ "$(jq -r '.name' <<<"$ONE")" = "$JOB_NAME" ] || fail "GET /api/jobs/$JOB_ID name mismatch: $ONE"

step "GET /api/calendar/events — cron expression expands onto the calendar"
FROM=$(date -u +%Y-%m-%dT%H:%M:%SZ)
TO=$(date -u -d '+400 days' +%Y-%m-%dT%H:%M:%SZ)
EVENTS=$(curl -fsS --max-time 10 "$BASE/api/calendar/events?from=$FROM&to=$TO")
MINE_EVENTS=$(jq --argjson id "$JOB_ID" '[.[] | select(.job_id==$id)] | length' <<<"$EVENTS")
# "0 0 1 1 *" fires exactly once in any 400-day window (next Jan 1).
[ "$MINE_EVENTS" = "1" ] || fail "expected 1 calendar occurrence for job $JOB_ID in 400d, got $MINE_EVENTS: $EVENTS"
OCC_START=$(jq -r --argjson id "$JOB_ID" '.[] | select(.job_id==$id) | .start' <<<"$EVENTS")
[[ "$OCC_START" == *-01-01T00:00:00* ]] || fail "occurrence should land on Jan 1 00:00, got $OCC_START"
echo "    next occurrence: $OCC_START"

step "POST /api/jobs/$JOB_ID/run — execute (echo only; writes nothing)"
RUN=$(curl -fsS --max-time 30 -X POST "$BASE/api/jobs/$JOB_ID/run")
[ "$(jq -r '.status' <<<"$RUN")" = "success" ] || fail "run did not succeed: $RUN"
RUN_OUT=$(jq -r '.output' <<<"$RUN")
[ "$(printf '%s' "$RUN_OUT" | tr -d '\n')" = "$MARKER" ] || fail "run output should be '$MARKER', got '$RUN_OUT'"
RUN_ID=$(jq -r '.id' <<<"$RUN")
echo "    run $RUN_ID: success, output '$MARKER'"

step "GET /api/jobs/$JOB_ID/runs — the run was persisted"
RUNS=$(curl -fsS --max-time 10 "$BASE/api/jobs/$JOB_ID/runs")
[ "$(jq 'length' <<<"$RUNS")" = "1" ] || fail "expected exactly 1 run in history, got: $RUNS"
[ "$(jq -r --argjson rid "$RUN_ID" '.[] | select(.id==$rid) | .status' <<<"$RUNS")" = "success" ] \
  || fail "run $RUN_ID not recorded as success: $RUNS"
jq -e --argjson rid "$RUN_ID" '.[] | select(.id==$rid) | .finished_at' <<<"$RUNS" >/dev/null \
  || fail "run $RUN_ID has no finished_at: $RUNS"

step "POST/DELETE /api/jobs/$JOB_ID/hold — hold round-trip"
HELD=$(curl -fsS --max-time 10 -X POST "$BASE/api/jobs/$JOB_ID/hold" \
  -H 'Content-Type: application/json' -d '{"days":7}')
HOLD_UNTIL=$(jq -r '.hold_until // "null"' <<<"$HELD")
[ "$HOLD_UNTIL" != "null" ] || fail "hold did not set hold_until: $HELD"
echo "    held until: $HOLD_UNTIL"
CLEARED=$(curl -fsS --max-time 10 -X DELETE "$BASE/api/jobs/$JOB_ID/hold")
[ "$(jq -r '.hold_until // "null"' <<<"$CLEARED")" = "null" ] || fail "hold was not cleared: $CLEARED"

step "PATCH /api/jobs/$JOB_ID — a non-enabled field really lands"
# PATCH used to decode into a struct carrying four fields and drop the rest
# behind a 200, so an edited command or prompt reported success and changed
# nothing. Assert on a re-read, not on the PATCH response.
curl -fsS --max-time 10 -X PATCH "$BASE/api/jobs/$JOB_ID" \
  -H 'Content-Type: application/json' -d '{"command":"echo patched-through"}' >/dev/null
REREAD=$(curl -fsS --max-time 10 "$BASE/api/jobs/$JOB_ID")
[ "$(jq -r '.command' <<<"$REREAD")" = "echo patched-through" ] \
  || fail "PATCHed command did not persist: $REREAD"
echo "    command edit persisted"

step "PATCH /api/jobs/$JOB_ID — an unrecognised field is rejected, not swallowed"
CODE=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -X PATCH "$BASE/api/jobs/$JOB_ID" \
  -H 'Content-Type: application/json' -d '{"promt":"typo"}')
[ "$CODE" = "400" ] || fail "PATCH with unknown field returned $CODE, want 400"
CODE=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -X PATCH "$BASE/api/jobs/$JOB_ID" \
  -H 'Content-Type: application/json' -d '{"schedule":"every tuesday"}')
[ "$CODE" = "400" ] || fail "PATCH with unparseable cron returned $CODE, want 400"
echo "    unknown field and bad cron both 400"

step "PATCH /api/jobs/$JOB_ID — disable"
PATCHED=$(curl -fsS --max-time 10 -X PATCH "$BASE/api/jobs/$JOB_ID" \
  -H 'Content-Type: application/json' -d '{"enabled":false}')
[ "$(jq -r '.enabled' <<<"$PATCHED")" = "false" ] || fail "job was not disabled: $PATCHED"
# A disabled job must drop off the calendar — proves the enabled flag is really
# read, not just echoed back.
EVENTS=$(curl -fsS --max-time 10 "$BASE/api/calendar/events?from=$FROM&to=$TO")
[ "$(jq --argjson id "$JOB_ID" '[.[] | select(.job_id==$id)] | length' <<<"$EVENTS")" = "0" ] \
  || fail "disabled job still appears in /api/calendar/events: $EVENTS"
echo "    disabled, and gone from the calendar"

step "DELETE /api/jobs/$JOB_ID — remove it"
CODE=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/jobs/$JOB_ID")
[ "$CODE" = "204" ] || fail "DELETE /api/jobs/$JOB_ID returned $CODE, want 204"
JOBS=$(curl -fsS --max-time 10 "$BASE/api/jobs")
[ "$(jq 'length' <<<"$JOBS")" = "0" ] || fail "job list not empty after delete: $JOBS"
CODE=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$BASE/api/jobs/$JOB_ID")
[ "$CODE" = "404" ] || fail "GET deleted job returned $CODE, want 404"
echo "    deleted, list empty, GET now 404"

step "process still alive after the whole run"
kill -0 "$SERVER_PID" 2>/dev/null || fail "scheduler died during the run"

step "SUCCESS"
echo "    scheduler booted, answered, executed a job, and cleaned up."
echo "    server log: $SERVER_LOG (removed unless E2E_KEEP=1)"
