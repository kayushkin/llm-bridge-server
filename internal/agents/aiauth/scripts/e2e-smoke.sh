#!/usr/bin/env bash
# Boot-and-answer smoke test for aiauth.
#
# Builds the CLI from THIS checkout and proves the resulting binary can actually
# BOOT and ANSWER. `go build` passing proves nothing of the sort: aiauth is a
# cobra program (cmd/aiauth/main.go) that assembles its command tree in main()
# via root.AddCommand(loginCmd(), statusCmd(), keyCmd(), refreshCmd()). cobra
# PANICS if two commands share a name — the CLI analog of the http.ServeMux
# route conflict that kills the HTTP services in this fleet at boot — and both
# `go build` and `go vet` call that green. Only running the binary catches it,
# and only running it catches a subcommand silently dropped from AddCommand.
#
# What is asserted, because a cobra CLI's real entrypoints are invisible to the
# compiler:
#   1. `aiauth --help` exits 0 and lists every subcommand main() registers. The
#      tree assembled without a panic; nothing dropped out of it.
#   2. `aiauth <sub> --help` exits 0 for each subcommand — the subcommand and its
#      flag set parse. --help is serviced before RunE, so this never dials out.
#   3. READ path: `aiauth status` against an EMPTY sandbox store answers
#      "No credentials configured." and exits 0 — it resolved its store path
#      (DefaultStore -> $HOME/.openclaw/.../auth-profiles.json), read it, and
#      reported. This is a parsed-content assertion, not just an exit code.
#   4. FAIL-LOUD path: `aiauth login <bad-provider>` exits non-zero and names the
#      rejected provider (main.go returns "unsupported provider: X" before any
#      OAuth begins — pure, no network). A resolve that silently succeeded, or
#      exited 0 on a bad provider, fails here.
#   5. FAIL-LOUD resolve: `aiauth key anthropic` against the empty store exits
#      non-zero with "no credentials" — ResolveKey walks env -> profiles and
#      errors on an empty store BEFORE any refresh/network (resolve.go:43). This
#      exercises the resolve path a green build never runs.
#
# HERMETICITY IS LOAD-BEARING, NOT A PRECAUTION.
#   aiauth's store is $HOME/.openclaw/agents/main/agent/auth-profiles.json, with
#   NO env override — HOME is the only lever. Run unsandboxed, `login`/`refresh`
#   would rewrite the LIVE OpenClaw auth-profiles.json (real Anthropic OAuth
#   tokens). This smoke therefore NEVER runs login/refresh/a real OAuth flow, runs
#   every invocation through `env -i` with HOME pointed at a throwaway dir, and —
#   from the OTHER side — fingerprints the real auth-profiles.json before the run
#   and re-checks it from the trap on EVERY exit path (a run that FAILED can still
#   have written on its way out). MODEL_STORE_URL is pinned at a closed port as a
#   second belt (syncModelStore only fires on login/refresh success, never here).
#   PATH is curated to the system dirs so the result cannot depend on which CLIs
#   happen to be installed.
#
# Exits 0 on success, non-zero on the first failing assertion. On failure the
# captured command output is dumped to stderr.
#
# Tunables:
#   E2E_DEAD_PORT — closed port that MODEL_STORE_URL is pinned to (default 19151;
#                   nothing must be listening on it).
#   E2E_KEEP      — set to "1" to leave $TMP_DIR around after the run.

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BIN_NAME="aiauth"
DEAD_PORT="${E2E_DEAD_PORT:-19151}"

for tool in go timeout md5sum; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "ERROR: required tool '$tool' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t aiauth-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
# HOME for the whole run. aiauth's store lives under $HOME/.openclaw; redirecting
# HOME means a stray write cannot reach the real profiles.
SANDBOX_HOME="$TMP_DIR/home"
RUN_DIR="$TMP_DIR/run"
BIN="$BIN_DIR/$BIN_NAME"
OUT="$TMP_DIR/out.txt"
mkdir -p "$BIN_DIR" "$SANDBOX_HOME" "$RUN_DIR"

# The LIVE store, from the OTHER side. If aiauth ever escaped its HOME sandbox,
# this fingerprint would change; we snapshot it now and re-verify from the trap.
LIVE_STORE="$HOME/.openclaw/agents/main/agent/auth-profiles.json"
live_fingerprint() {
  if [ -e "$LIVE_STORE" ]; then md5sum "$LIVE_STORE" | awk '{print $1}'; else echo "ABSENT"; fi
}
LIVE_STORE_BEFORE="$(live_fingerprint)"

# The binary UNDER TEST always runs through this wrapper: curated PATH, a
# throwaway HOME so login/refresh/status/key cannot touch the real auth-profiles,
# and MODEL_STORE_URL pinned to a closed port so no sync path can reach a live
# service. The build below keeps the ambient env so module/build caches are the
# real ones and nothing re-downloads.
run_aiauth() {
  timeout 15 env -i \
    PATH="/usr/bin:/bin" \
    HOME="$SANDBOX_HOME" \
    MODEL_STORE_URL="http://127.0.0.1:$DEAD_PORT" \
    "$BIN" "$@"
}

DUMPED=0
dump_out() {
  [ "$DUMPED" = "1" ] && return 0
  DUMPED=1
  if [ -s "$OUT" ]; then
    echo "----- last command output -----" >&2
    cat "$OUT" >&2
    echo "-------------------------------" >&2
  fi
}

cleanup() {
  local status=$?
  # Hermeticity, checked on EVERY exit path — a run that failed can still have
  # corrupted the live store on its way out.
  local after; after="$(live_fingerprint)"
  if [ "$after" != "$LIVE_STORE_BEFORE" ]; then
    echo "FAIL: the run CHANGED the live store $LIVE_STORE (before=$LIVE_STORE_BEFORE after=$after)" >&2
    status=1
  fi
  [ "$status" -ne 0 ] && dump_out
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
  dump_out
  exit 1
}

step "build $BIN_NAME from $REPO_DIR"
cd "$REPO_DIR"
# Main package is at ./cmd/aiauth. Pure Go (spf13/cobra + anthropic SDK) —
# CGO_ENABLED=0 makes a cgo dependency fail at the build step instead of shipping
# a binary that compiles green and dies at runtime.
CGO_ENABLED=0 go build -o "$BIN" ./cmd/aiauth
echo "    binary: $BIN ($(ls -lh "$BIN" | awk '{print $5}'))"

# Run from a temp CWD so any CWD-relative write lands in the sandbox, never the
# checkout.
cd "$RUN_DIR"

step "$BIN_NAME --help — the cobra command tree assembled (no panic)"
if ! run_aiauth --help >"$OUT" 2>&1; then
  fail "'$BIN_NAME --help' exited non-zero — the command tree did not assemble"
fi
EXPECTED_CMDS="login status key refresh"
for c in $EXPECTED_CMDS; do
  grep -qE "^[[:space:]]+$c[[:space:]]" "$OUT" \
    || fail "'$BIN_NAME --help' does not list the '$c' subcommand — tree is incomplete"
done
echo "    all subcommands present: $EXPECTED_CMDS"

step "$BIN_NAME <sub> --help — every subcommand and its flags parse"
for c in $EXPECTED_CMDS; do
  # --help is serviced before RunE, so this never runs OAuth / dials out.
  run_aiauth "$c" --help >"$OUT" 2>&1 \
    || fail "'$BIN_NAME $c --help' exited non-zero — subcommand '$c' is broken"
done
echo "    help resolves for all $BIN_NAME subcommands"

step "$BIN_NAME status — reads the sandbox store and answers"
# Empty sandbox store => "No credentials configured." + exit 0. Proves the store
# path resolved under the sandbox HOME and the read path works.
if ! run_aiauth status >"$OUT" 2>&1; then
  fail "'$BIN_NAME status' exited non-zero against an empty store"
fi
grep -qi "No credentials configured" "$OUT" \
  || fail "'$BIN_NAME status' did not report the empty-store state: $(cat "$OUT")"
echo "    status answered: $(head -1 "$OUT")"

step "$BIN_NAME login <bad-provider> — a bad provider fails LOUDLY, no OAuth"
set +e
run_aiauth login notaprovider >"$OUT" 2>&1
rc=$?
set -e
[ "$rc" -eq 124 ] && fail "'$BIN_NAME login notaprovider' HUNG — a bad provider reached a network path"
[ "$rc" -ne 0 ] || fail "'$BIN_NAME login notaprovider' exited 0 — a bad provider was accepted"
grep -qi "unsupported provider" "$OUT" \
  || fail "the rejection does not name the bad provider: $(cat "$OUT")"
echo "    rejected loudly: $(head -1 "$OUT")"

step "$BIN_NAME key anthropic — resolve fails LOUDLY on an empty store, no refresh"
set +e
run_aiauth key anthropic >"$OUT" 2>&1
rc=$?
set -e
[ "$rc" -eq 124 ] && fail "'$BIN_NAME key anthropic' HUNG — it reached a refresh/network path on an empty store"
[ "$rc" -ne 0 ] || fail "'$BIN_NAME key anthropic' exited 0 with no credentials — a missing key was reported as success"
grep -qiE "no credentials|no valid credentials" "$OUT" \
  || fail "the failure does not read as a missing-credential error: $(cat "$OUT")"
echo "    failed loudly: $(head -1 "$OUT")"

step "the run was hermetic"
# The live store check runs from the trap on every exit; here we prove nothing
# escaped into the checkout, and that any write the binary DID make landed in the
# sandbox (not the real ~/.openclaw).
[ ! -e "$REPO_DIR/.openclaw" ] || fail "the run wrote .openclaw INTO THE CHECKOUT at $REPO_DIR/.openclaw"
echo "    live store untouched (verified from trap); wrote only under $TMP_DIR"

step "SUCCESS — $BIN_NAME boots and answers"
