#!/usr/bin/env bash
# Boot-and-answer smoke test for llm-bridge-runner.
#
# llm-bridge-runner is NOT a harness CLI. It has no -discover and no JSON-RPC
# loop; it registers a machine with an llm-bridge-server over an outbound
# WebSocket. So the harness-CLI smoke shape does not apply — its entrypoints are
# `enroll` (a one-time credential exchange that WRITES a durable token to disk)
# and `run` (the long-lived default).
#
# 🔴 ENROLMENT IS A STATE-CHANGING WRITE ON BOTH SIDES. It mints a durable
# per-machine token on the server and persists it locally. This smoke therefore
# NEVER touches the live llm-bridge-server: it stands up a stub server on a temp
# port that speaks just enough of POST /api/runner/enroll, and it redirects the
# config path into a sandbox. Pointing this at :8160 would enrol a phantom
# machine into the real fleet on every nightly run.
#
# What is asserted, and why each one can fail while `go build` is green:
#
#   1. -version / --help      the two zero-side-effect entrypoints answer.
#   2. `run` with NO config   must FAIL FAST AND LOUD (exit 1, message naming
#                             `enroll`) rather than hang or panic. This is the
#                             state a freshly-installed runner is in, and a hang
#                             here wedges the systemd unit silently.
#   3. `enroll` missing flags exits 2 without reaching the network.
#   4. `enroll` unreachable   errors cleanly AND WRITES NO CONFIG. A partially
#                             written config is worse than none: `run` would then
#                             dial forever with a token the server never issued.
#   5. `enroll` HTTP 500      surfaces the server's status, does not write config.
#   6. `enroll` happy path    against the stub: writes config.json at MODE 0600
#                             (it holds a bearer token — the mode is the contract),
#                             with the fields the server returned. This is the
#                             only assertion that exercises Enroll end-to-end:
#                             build request → POST → parse → SaveConfig.
#   7. `run` WITH a config    boots the production path, logs the dial, reports
#                             the disconnect against a closed port, and shuts
#                             down cleanly on SIGTERM. A runner that cannot be
#                             stopped is a runner that cannot be restarted.
#
# HERMETICITY IS AN ASSERTION HERE, NOT A PRECAUTION.
# PATH is curated to the system dirs because DetectHarnesses() (harness.go)
# exec.LookPath()s every harness wrapper on PATH and runs each with -version. A
# smoke whose enrol payload depends on which wrappers happen to be installed is
# not a guard, it is a coin flip — and several of those wrappers open live state
# databases when executed. The live config file is checksummed BEFORE the run and
# rechecked from the trap on EVERY exit path, including the failing ones: an
# assertion that only runs on success cannot tell you that the run which just
# failed also overwrote your real runner token on its way out.
#
# Exits 0 on success, non-zero on the first failing assertion.
#
# Tunables:
#   E2E_KEEP        — "1" leaves $TMP_DIR around after the run
#   E2E_STUB_PORT   — stub server listen port (default 19117)

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BIN_NAME="llm-bridge-runner"
STUB_PORT="${E2E_STUB_PORT:-19117}"
STUB_BASE="http://127.0.0.1:$STUB_PORT"

for tool in go jq timeout python3; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "ERROR: required tool '$tool' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t runner-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
SANDBOX_HOME="$TMP_DIR/home"
BIN="$BIN_DIR/$BIN_NAME"
# Where the runner under test is told to keep its config. Honored via
# $LLMBRIDGE_RUNNER_CONFIG (config.go:configPath). Deliberately a path the
# default could never produce, so assertion 6 can only pass if the override is
# actually read — a hermeticity fix is only real if the test stops relying on
# the workaround.
SANDBOX_CONFIG="$TMP_DIR/runner-config/config.json"
STUB_LOG="$TMP_DIR/stub.log"
STUB_REQUESTS="$TMP_DIR/stub-requests.jsonl"
mkdir -p "$BIN_DIR" "$SANDBOX_HOME"

# The real config this runner would read/write if the sandbox leaked. It holds a
# durable bearer token. In the guard's clean-clone environment HOME is already
# scratch and this does not exist; when a human runs this smoke by hand it is the
# real one, and that is exactly the case worth protecting.
#
# 🔴 SANDBOXING $HOME IS NOT ENOUGH FOR THIS BINARY, and that is not obvious.
# configPath() falls back to user.Current().HomeDir, which reads /etc/passwd and
# IGNORES $HOME. Only $LLMBRIDGE_RUNNER_CONFIG actually redirects it. The seed
# reconcilers (NewClient -> initReconcilers) MkdirAll this same directory, so a
# run that forgets the override creates and writes the REAL config dir even with
# HOME pointed at a scratch path. So the guard below watches the DIRECTORY, not
# just config.json: the first leak this smoke ever sprang left the file absent
# and the directory behind, and a file-only check waved it through.
LIVE_CONFIG_DIR="$(cd ~ 2>/dev/null && pwd)/.config/llm-bridge-runner"
LIVE_CONFIG="$LIVE_CONFIG_DIR/config.json"
LIVE_CONFIG_BEFORE=""
LIVE_CONFIG_EXISTED=0
LIVE_DIR_EXISTED=0
[ -d "$LIVE_CONFIG_DIR" ] && LIVE_DIR_EXISTED=1
if [ -f "$LIVE_CONFIG" ]; then
  LIVE_CONFIG_EXISTED=1
  LIVE_CONFIG_BEFORE="$(sha256sum "$LIVE_CONFIG" | cut -d' ' -f1)"
fi

check_live_config_untouched() {
  # Runs on EVERY exit path. Two distinct leaks to catch: overwriting an existing
  # config (the token is replaced) and CREATING one where there was none (a
  # phantom enrolment). Both mean $LLMBRIDGE_RUNNER_CONFIG was not honored.
  if [ "$LIVE_CONFIG_EXISTED" = "1" ]; then
    local after
    after="$(sha256sum "$LIVE_CONFIG" 2>/dev/null | cut -d' ' -f1 || true)"
    if [ "$LIVE_CONFIG_BEFORE" != "$after" ]; then
      echo "" >&2
      echo "!!! THIS SMOKE MODIFIED THE LIVE RUNNER CONFIG $LIVE_CONFIG" >&2
      echo "!!! \$LLMBRIDGE_RUNNER_CONFIG was not honored. That file holds the" >&2
      echo "!!! durable bearer token this machine authenticates with." >&2
      return 1
    fi
  elif [ -e "$LIVE_CONFIG" ]; then
    echo "" >&2
    echo "!!! THIS SMOKE CREATED A LIVE RUNNER CONFIG AT $LIVE_CONFIG" >&2
    echo "!!! \$LLMBRIDGE_RUNNER_CONFIG was not honored, so the sandboxed enrol" >&2
    echo "!!! wrote a real one. Delete it — it is a phantom machine identity." >&2
    return 1
  fi
  # The directory, not only the file. The seed reconcilers MkdirAll it on every
  # NewClient(), so a path-resolution leak shows up here FIRST — with the config
  # file still absent. Checking only config.json missed exactly that.
  if [ "$LIVE_DIR_EXISTED" = "0" ] && [ -d "$LIVE_CONFIG_DIR" ]; then
    echo "" >&2
    echo "!!! THIS SMOKE CREATED THE LIVE CONFIG DIR $LIVE_CONFIG_DIR" >&2
    echo "!!! Something in the runner resolves that path from /etc/passwd rather" >&2
    echo "!!! than from \$LLMBRIDGE_RUNNER_CONFIG, so the sandbox does not hold." >&2
    return 1
  fi
  return 0
}

cleanup() {
  local status=$?
  [ -n "${STUB_PID:-}" ] && kill "$STUB_PID" 2>/dev/null || true
  [ -n "${RUN_PID:-}" ] && kill "$RUN_PID" 2>/dev/null || true
  check_live_config_untouched || status=1
  if [ "$status" -ne 0 ] && [ -f "$STUB_LOG" ]; then
    echo "----- stub.log -----" >&2
    cat "$STUB_LOG" >&2
  fi
  if [ "${E2E_KEEP:-}" = "1" ]; then
    echo "[e2e] keeping $TMP_DIR"
  else
    rm -rf "$TMP_DIR"
  fi
  return "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# Run the runner under test. `env -i` is the point: an ambient PATH would let
# DetectHarnesses() reach the real installed harness wrappers, and an ambient
# HOME/XDG_CONFIG_HOME would let it reach the live config.
RUNNER_TIMEOUT="${RUNNER_TIMEOUT:-30}"
run_runner() {
  timeout "$RUNNER_TIMEOUT" env -i \
    HOME="$SANDBOX_HOME" \
    PATH="/usr/bin:/bin" \
    LLMBRIDGE_RUNNER_CONFIG="$SANDBOX_CONFIG" \
    "$BIN" "$@"
}

step "build $BIN_NAME from $REPO_DIR"
cd "$REPO_DIR"
# CGO_ENABLED=0: this repo is pure Go. If a cgo SQLite driver is ever pulled in,
# fail here at the build rather than ship a binary that compiles green and dies
# opening its own database (the noteboard/marginalia FTS5 class).
CGO_ENABLED=0 go build -o "$BIN" . || fail "go build failed"
[ -x "$BIN" ] || fail "no binary at $BIN after build"

# ---------------------------------------------------------------------------
step "write the stub llm-bridge-server"
# Speaks exactly one route: POST /api/runner/enroll. Behavior is selected by the
# path prefix so a single process can serve the happy path and the 500 case
# without a restart. Records every request body so the payload shape can be
# asserted rather than assumed.
cat >"$TMP_DIR/stub_server.py" <<'PYEOF'
"""Stub llm-bridge-server: the smallest surface llm-bridge-runner's enroll calls.

/api/runner/enroll        -> 200 with a minted token (the happy path)
/reject/api/runner/enroll -> 500, to prove the runner surfaces the status and
                             does not persist anything on a rejected enrolment.
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1])
REQUEST_LOG = sys.argv[2]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b""
        with open(REQUEST_LOG, "a") as fh:
            fh.write(json.dumps({"path": self.path, "body": raw.decode("utf-8", "replace")}) + "\n")

        if self.path.startswith("/reject/"):
            self._send(500, b"stub: enrolment deliberately rejected")
            return
        if not self.path.endswith("/api/runner/enroll"):
            self._send(404, b"stub: no such route")
            return

        req = json.loads(raw or b"{}")
        body = json.dumps({
            "machine_id": "mach_stub_001",
            "machine_name": req.get("machine_name", ""),
            "runner_token": "tok_stub_deadbeef",
        }).encode()
        self._send(200, body, "application/json")

    def do_GET(self):
        # Readiness probe only.
        self._send(200, b"ok")

    def _send(self, code, body, ctype="text/plain"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        sys.stderr.write("stub: " + fmt % args + "\n")


HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
PYEOF

step "launch the stub server on :$STUB_PORT"
python3 "$TMP_DIR/stub_server.py" "$STUB_PORT" "$STUB_REQUESTS" >"$STUB_LOG" 2>&1 &
STUB_PID=$!

# Poll for readiness — never sleep. Abort the instant the pid dies, so a stub
# that failed to bind surfaces as a clear failure instead of a mystery timeout.
STUB_READY=0
for _ in $(seq 1 50); do
  if ! kill -0 "$STUB_PID" 2>/dev/null; then
    fail "stub server exited during startup (see log below)"
  fi
  if curl -fsS --max-time 1 "$STUB_BASE/" >/dev/null 2>&1; then
    STUB_READY=1
    break
  fi
  sleep 0.1
done
[ "$STUB_READY" = "1" ] || fail "stub server did not come up on $STUB_BASE"

# ---------------------------------------------------------------------------
step "1. -version and --help answer without side effects"
VERSION_OUT="$(run_runner -version)" || fail "-version exited non-zero"
[ -n "$VERSION_OUT" ] || fail "-version printed nothing"

# --help writes usage to stderr and exits 0.
HELP_OUT="$(run_runner --help 2>&1)" || fail "--help exited non-zero"
case "$HELP_OUT" in
  *enroll*) ;;
  *) fail "--help does not document the enroll subcommand; got: $HELP_OUT" ;;
esac

[ -e "$SANDBOX_CONFIG" ] && fail "-version/--help wrote a config file; they must have no side effects"

# ---------------------------------------------------------------------------
step "2. \`run\` with no saved config fails fast and loud"
# This is the state of a freshly-installed runner. It must NOT hang (a hung
# runner wedges its systemd unit while looking alive) and must NOT panic. The
# error has to name the remedy, because the operator reading it has no other clue.
set +e
RUN_OUT="$(run_runner run 2>&1)"
RUN_STATUS=$?
set -e
[ "$RUN_STATUS" -eq 124 ] && fail "\`run\` with no config HUNG (timed out) — it must fail fast"
[ "$RUN_STATUS" -eq 1 ] || fail "\`run\` with no config exited $RUN_STATUS, want 1. Output: $RUN_OUT"
case "$RUN_OUT" in
  *panic*) fail "\`run\` with no config PANICKED: $RUN_OUT" ;;
esac
case "$RUN_OUT" in
  *enroll*) ;;
  *) fail "\`run\` error does not tell the operator to enroll; got: $RUN_OUT" ;;
esac

# The bare default invocation (no subcommand) routes to runService too — assert
# the same, because that is what the systemd unit actually execs.
set +e
BARE_OUT="$(run_runner 2>&1)"
BARE_STATUS=$?
set -e
[ "$BARE_STATUS" -eq 1 ] || fail "bare invocation exited $BARE_STATUS, want 1. Output: $BARE_OUT"

# ---------------------------------------------------------------------------
step "3. \`enroll\` without required flags exits 2 and never reaches the network"
REQUESTS_BEFORE=0
[ -f "$STUB_REQUESTS" ] && REQUESTS_BEFORE="$(wc -l <"$STUB_REQUESTS")"

set +e
MISSING_OUT="$(run_runner enroll 2>&1)"
MISSING_STATUS=$?
set -e
[ "$MISSING_STATUS" -eq 2 ] || fail "\`enroll\` with no flags exited $MISSING_STATUS, want 2. Output: $MISSING_OUT"

set +e
run_runner enroll --server "$STUB_BASE" >/dev/null 2>&1
NOPASS_STATUS=$?
set -e
[ "$NOPASS_STATUS" -eq 2 ] || fail "\`enroll --server\` without --passphrase exited $NOPASS_STATUS, want 2"

REQUESTS_AFTER=0
[ -f "$STUB_REQUESTS" ] && REQUESTS_AFTER="$(wc -l <"$STUB_REQUESTS")"
[ "$REQUESTS_BEFORE" = "$REQUESTS_AFTER" ] || fail "\`enroll\` with missing flags still hit the server — validation must precede the POST"

# ---------------------------------------------------------------------------
step "4. \`enroll\` against an unreachable server errors cleanly and writes NO config"
# Port 1 is reserved and nothing listens on it. A partially written config here
# would be worse than none: `run` would dial forever with a token the server
# never issued, and the operator would see a runner that looks enrolled.
set +e
UNREACH_OUT="$(run_runner enroll --server "http://127.0.0.1:1" --passphrase "pw" 2>&1)"
UNREACH_STATUS=$?
set -e
[ "$UNREACH_STATUS" -eq 124 ] && fail "\`enroll\` against an unreachable server HUNG"
[ "$UNREACH_STATUS" -eq 1 ] || fail "\`enroll\` against an unreachable server exited $UNREACH_STATUS, want 1"
case "$UNREACH_OUT" in
  *panic*) fail "\`enroll\` against an unreachable server PANICKED: $UNREACH_OUT" ;;
  *"enrollment failed"*) ;;
  *) fail "\`enroll\` failure not reported as an enrollment failure; got: $UNREACH_OUT" ;;
esac
[ -e "$SANDBOX_CONFIG" ] && fail "a FAILED enrolment wrote a config file at $SANDBOX_CONFIG"

# ---------------------------------------------------------------------------
step "5. \`enroll\` rejected with HTTP 500 surfaces the status and writes NO config"
set +e
REJECT_OUT="$(run_runner enroll --server "$STUB_BASE/reject" --passphrase "pw" 2>&1)"
REJECT_STATUS=$?
set -e
[ "$REJECT_STATUS" -eq 1 ] || fail "rejected enrolment exited $REJECT_STATUS, want 1. Output: $REJECT_OUT"
case "$REJECT_OUT" in
  *500*) ;;
  *) fail "rejected enrolment did not surface the HTTP status; got: $REJECT_OUT" ;;
esac
[ -e "$SANDBOX_CONFIG" ] && fail "a REJECTED enrolment wrote a config file at $SANDBOX_CONFIG"

# ---------------------------------------------------------------------------
step "6. \`enroll\` happy path writes a 0600 config with the server's fields"
ENROLL_OUT="$(run_runner enroll --server "$STUB_BASE" --passphrase "pw" --name "smoke-machine" 2>&1)" \
  || fail "happy-path enroll exited non-zero: $ENROLL_OUT"

[ -f "$SANDBOX_CONFIG" ] || fail "successful enrol wrote no config at \$LLMBRIDGE_RUNNER_CONFIG ($SANDBOX_CONFIG) — the override is not honored"

# The file holds a durable bearer token. config.go says "file mode 0600 —
# sensitive". Assert the mode, not the comment.
CONFIG_MODE="$(stat -c '%a' "$SANDBOX_CONFIG")"
[ "$CONFIG_MODE" = "600" ] || fail "config.json mode is $CONFIG_MODE, want 600 — it holds a bearer token"

# Parse the body, don't trust the exit code. Compare in the parent shell: a
# `fail` inside $(...) only kills the subshell.
GOT_TOKEN="$(jq -r '.runner_token' "$SANDBOX_CONFIG")"
GOT_MACHINE_ID="$(jq -r '.machine_id' "$SANDBOX_CONFIG")"
GOT_NAME="$(jq -r '.machine_name' "$SANDBOX_CONFIG")"
GOT_SERVER="$(jq -r '.server_url' "$SANDBOX_CONFIG")"
[ "$GOT_TOKEN" = "tok_stub_deadbeef" ] || fail "config runner_token is '$GOT_TOKEN', want the token the server minted"
[ "$GOT_MACHINE_ID" = "mach_stub_001" ] || fail "config machine_id is '$GOT_MACHINE_ID', want the id the server assigned"
[ "$GOT_NAME" = "smoke-machine" ] || fail "config machine_name is '$GOT_NAME', want the --name we passed"
[ "$GOT_SERVER" = "$STUB_BASE" ] || fail "config server_url is '$GOT_SERVER', want $STUB_BASE"

# The request the runner actually sent. Under the curated PATH no harness
# wrappers are reachable, so available_harnesses must be empty — that is what
# makes this smoke's payload reproducible instead of a function of what happens
# to be installed on the box.
LAST_REQUEST="$(tail -1 "$STUB_REQUESTS")"
SENT_BODY="$(printf '%s' "$LAST_REQUEST" | jq -r '.body')"
SENT_PASSPHRASE="$(printf '%s' "$SENT_BODY" | jq -r '.passphrase')"
SENT_VERSION="$(printf '%s' "$SENT_BODY" | jq -r '.runner_version')"
SENT_HARNESSES="$(printf '%s' "$SENT_BODY" | jq -r '.available_harnesses | length')"
[ "$SENT_PASSPHRASE" = "pw" ] || fail "enrol request carried passphrase '$SENT_PASSPHRASE', want 'pw'"
[ -n "$SENT_VERSION" ] && [ "$SENT_VERSION" != "null" ] || fail "enrol request carried no runner_version"
[ "$SENT_HARNESSES" = "0" ] || fail "enrol request advertised $SENT_HARNESSES harnesses under a curated PATH — DetectHarnesses() reached outside the sandbox"

# ---------------------------------------------------------------------------
step "7. \`run\` with a config boots, reports the disconnect, and stops on SIGTERM"
# Repoint the saved config at a closed port: the runner must dial, fail, log the
# disconnect and back off — not crash. Then SIGTERM must actually stop it. A
# runner that ignores SIGTERM cannot be restarted by systemd, which is how a
# stale binary survives a deploy.
jq '.server_url = "http://127.0.0.1:1"' "$SANDBOX_CONFIG" >"$TMP_DIR/closed-port-config.json"
RUN_LOG="$TMP_DIR/run.log"

env -i HOME="$SANDBOX_HOME" PATH="/usr/bin:/bin" \
  LLMBRIDGE_RUNNER_CONFIG="$TMP_DIR/closed-port-config.json" \
  "$BIN" run >"$RUN_LOG" 2>&1 &
RUN_PID=$!

# Poll for the runner to reach its dial/disconnect cycle. Abort if it dies: an
# exit here means the production entrypoint cannot boot from a valid config.
RUN_DIALED=0
for _ in $(seq 1 100); do
  if ! kill -0 "$RUN_PID" 2>/dev/null; then
    echo "----- run.log -----" >&2
    cat "$RUN_LOG" >&2
    fail "\`run\` exited on its own with a valid config — it should retry, not die"
  fi
  if grep -q "disconnected" "$RUN_LOG" 2>/dev/null; then
    RUN_DIALED=1
    break
  fi
  sleep 0.1
done
[ "$RUN_DIALED" = "1" ] || fail "\`run\` never reported a disconnect against a closed port within 10s"

grep -q "\[runner\] starting" "$RUN_LOG" || fail "\`run\` did not log its startup banner"
grep -q "\[runner\] dialing" "$RUN_LOG" || fail "\`run\` never logged a dial attempt"

kill -TERM "$RUN_PID"
STOPPED=0
for _ in $(seq 1 100); do
  if ! kill -0 "$RUN_PID" 2>/dev/null; then
    STOPPED=1
    break
  fi
  sleep 0.1
done
if [ "$STOPPED" != "1" ]; then
  kill -9 "$RUN_PID" 2>/dev/null || true
  fail "\`run\` did not exit within 10s of SIGTERM — systemd could not restart this runner"
fi
wait "$RUN_PID" 2>/dev/null || true
RUN_PID=""

grep -q "shutting down" "$RUN_LOG" || fail "\`run\` exited on SIGTERM without logging a clean shutdown"

# ---------------------------------------------------------------------------
step "8. the sandbox held"
# `run` builds the seed reconcilers, which MkdirAll their sidecar directory. That
# directory must be the one $LLMBRIDGE_RUNNER_CONFIG names — assert it POSITIVELY,
# here, rather than only asserting the live path stayed clean. A negative check
# alone passes just as happily when the code path never ran at all.
SIDECAR_DIR="$(dirname "$TMP_DIR/closed-port-config.json")"
[ -d "$SIDECAR_DIR" ] || fail "the seed reconcilers never created their sidecar dir — this assertion is not exercising anything"

# And nothing may have landed under the sandboxed HOME. Note this check is NOT
# what protects the live tree: configPath() falls back to user.Current().HomeDir,
# which ignores $HOME entirely, so a leak lands in the REAL home rather than here.
# That case is caught by check_live_config_untouched, from the trap, on every exit
# path. This one only catches a NEW path that does honor $HOME.
if [ -n "$(ls -A "$SANDBOX_HOME" 2>/dev/null)" ]; then
  fail "the runner wrote to the sandboxed \$HOME: $(ls -A "$SANDBOX_HOME")"
fi

# Check the live tree HERE too, not only from the trap. The trap runs after this
# banner and can still fail the run — but by then stdout already said "OK", and a
# human reading the log believes the last line, not the exit code. A smoke whose
# output disagrees with its exit status is the same defect as one that prints
# FAIL and exits 0, just pointed the other way.
check_live_config_untouched || fail "the run leaked into the live config tree (see above)"

echo ""
echo "e2e smoke test OK ($BIN_NAME)"
