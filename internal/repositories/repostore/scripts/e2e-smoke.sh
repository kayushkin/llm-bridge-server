#!/usr/bin/env bash
# Boot-and-answer smoke test for repo-store.
#
# Builds cmd/repo-store from THIS checkout, boots it against a throwaway SQLite
# file on a throwaway port, and drives real HTTP routes. Proves the committed
# tree produces a binary that BOOTS (a Go 1.22+ ServeMux route conflict panics
# at registration time — it compiles green and dies on start) and ANSWERS
# (writes a repo, reads it back, and runs real filesystem detection).
#
# Never touches live state: the live service runs on :8306 against
# ~/.config/repo-store/repo-store.db. REPO_STORE_DATA_DIR is what keeps this
# hermetic — Open("") falls back to that live path and migrates it. HOME is
# redirected too, belt and braces (the unit sets HOME explicitly and relies on
# it for the default path, so a regression that ignores the env var would land
# in the real DB).
#
# Detection is exercised against a fixture tree built inside $TMP_DIR — never
# against a real repo on the host.
#
# Exits 0 on success, non-zero on the first failing assertion; dumps the server
# log to stderr on failure.
#
# Tunables:
#   E2E_PORT — repo-store listen port (default 19114)
#   E2E_KEEP — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19114}"
BASE="http://127.0.0.1:$PORT"

# repo-store uses mattn/go-sqlite3, which is cgo — a C compiler is mandatory or
# `go build` fails with a confusing linker error under the nightly job's minimal env.
for bin in go curl jq cc; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t repo-store-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
FIXTURE="$TMP_DIR/fixture-go-repo"
BODY="$TMP_DIR/body.json"
mkdir -p "$BIN_DIR" "$DATA_DIR" "$FIXTURE" "$TMP_DIR/fake-home"

NAME="e2e-smoke-$$"

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
  if [ -f "$TMP_DIR/server.log" ]; then
    echo "----- server.log -----" >&2
    cat "$TMP_DIR/server.log" >&2
  fi
  exit 1
}

req() {
  local method="$1" path="$2" data="${3:-}"
  local args=(-sS -o "$BODY" -w '%{http_code}' --max-time 15 -X "$method" "$BASE$path")
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' -d "$data")
  fi
  curl "${args[@]}"
}

expect() {
  local want="$1" got="$2" what="$3"
  if [ "$want" != "$got" ]; then
    echo "----- response body -----" >&2
    cat "$BODY" >&2
    echo >&2
    fail "$what: expected HTTP $want, got $got"
  fi
}

jget() { jq -r "$1" "$BODY"; }

assert_eq() {
  [ "$1" = "$2" ] || fail "$3: expected '$1', got '$2'"
}

# assert_has_tag ARRAY_FILTER TAG WHAT — the tag lists are unordered, so never
# compare them positionally.
assert_has_tag() {
  local got
  got="$(jq -r "[$1] | index(\"$2\") // \"MISSING\"" "$BODY")"
  [ "$got" != "MISSING" ] || {
    echo "----- response body -----" >&2
    cat "$BODY" >&2
    echo >&2
    fail "$3: expected tag '$2' to be present"
  }
}

# ============================================================================
step "build cmd/repo-store from $REPO_DIR"
cd "$REPO_DIR"
CGO_ENABLED=1 go build -o "$BIN_DIR/repo-store" ./cmd/repo-store
echo "    binary: $(ls -lh "$BIN_DIR/repo-store" | awk '{print $5}')"

step "build a fixture repo for detection to chew on"
# A Go module with a frontend package.json — repodetect should roll these up to
# the "go" and "web"/"frontend" tags. Entirely inside $TMP_DIR.
printf 'module example.com/fixture\n\ngo 1.22\n' >"$FIXTURE/go.mod"
printf '{"name":"fixture","dependencies":{"react":"^19.0.0"}}\n' >"$FIXTURE/package.json"
echo "    fixture: $FIXTURE"

# ============================================================================
step "boot repo-store on :$PORT against a throwaway data dir"
REPO_STORE_ADDR=":$PORT" \
REPO_STORE_DATA_DIR="$DATA_DIR" \
HOME="$TMP_DIR/fake-home" \
  "$BIN_DIR/repo-store" >>"$TMP_DIR/server.log" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID  data dir: $DATA_DIR"

# Poll — never sleep-and-hope. Abort the instant the pid dies, so a route
# registration panic surfaces as a clear failure instead of a mystery timeout.
OK=""
for _ in $(seq 1 75); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    fail "repo-store exited during startup (route-registration panic? db migration?)"
  fi
  if curl -fsS --max-time 2 -o /dev/null "$BASE/health" 2>/dev/null; then OK=1; break; fi
  sleep 0.2
done
[ -n "$OK" ] || fail "repo-store did not answer /health on $BASE within ~15s"

step "GET /health"
CODE=$(req GET /health); expect 200 "$CODE" "/health"
assert_eq ok "$(jget '.status')" "health status"

step "GET /repos — fresh db is empty"
CODE=$(req GET /repos); expect 200 "$CODE" "GET /repos"
assert_eq 0 "$(jget 'length')" "fresh db repo count"

# ============================================================================
step "POST /detect — stateless detection against the fixture tree"
CODE=$(req POST /detect "{\"path\":\"$FIXTURE\"}")
expect 200 "$CODE" "POST /detect"
assert_has_tag '.tags[]' go "detect found the go.mod"
[ "$(jget '.signature | type')" = "object" ] || fail "/detect returned no signature object"
echo "    tags: $(jget '.tags | join(", ")')"

step "POST /detect — a path that does not exist fails loudly"
CODE=$(req POST /detect '{"path":"/nonexistent/definitely/not/here"}')
[ "$CODE" = 400 ] || [ "$CODE" = 500 ] \
  || fail "POST /detect on a missing path: expected 4xx/5xx, got $CODE"

step "POST /detect persisted nothing"
CODE=$(req GET /repos); expect 200 "$CODE" "GET /repos after /detect"
assert_eq 0 "$(jget 'length')" "/detect is stateless — it must not create a repo"

# ============================================================================
step "POST /repos — create a repo, with detection running over the fixture path"
# NB: when `path` is set the server DISCARDS body-supplied `tags` and replaces
# them with the detected signature. Manual tags therefore go in `tag_overrides`,
# and `effective_tags` is the union — computed on read, never stored.
CODE=$(req POST /repos \
  "{\"name\":\"$NAME\",\"path\":\"$FIXTURE\",\"description\":\"created by e2e-smoke\",\"tag_overrides\":[\"e2e-manual\"]}")
expect 201 "$CODE" "POST /repos"
REPO_ID="$(jget '.id')"
[ -n "$REPO_ID" ] && [ "$REPO_ID" != "null" ] || fail "POST /repos returned no id"
assert_eq "$NAME" "$(jget '.name')" "created repo name"
echo "    repo id: $REPO_ID"

step "GET /repos/by-name/{name} — effective_tags is the union of detected + manual"
CODE=$(req GET "/repos/by-name/$NAME"); expect 200 "$CODE" "GET /repos/by-name/{name}"
assert_eq "$NAME" "$(jget '.name')" "read-back name"
assert_eq "created by e2e-smoke" "$(jget '.description')" "read-back description"
assert_has_tag '.tags[]'           go         "detected tag survived the round trip"
assert_has_tag '.tag_overrides[]'  e2e-manual "manual override survived the round trip"
assert_has_tag '.effective_tags[]' go         "effective_tags includes the detected tag"
assert_has_tag '.effective_tags[]' e2e-manual "effective_tags includes the manual override"

step "GET /repos/{id} — same repo by id"
CODE=$(req GET "/repos/$REPO_ID"); expect 200 "$CODE" "GET /repos/{id}"
assert_eq "$NAME" "$(jget '.name')" "read-back name by id"

step "GET /by-path?path= — reverse lookup"
CODE=$(req GET "/by-path?path=$(printf '%s' "$FIXTURE" | jq -sRr @uri)")
expect 200 "$CODE" "GET /by-path"
assert_eq "$NAME" "$(jget '.name')" "by-path found the repo"

step "GET /by-path with no path param — 400, not a panic"
CODE=$(req GET /by-path); expect 400 "$CODE" "GET /by-path with no path"

step "GET /repos?tag= — the tag filter matches on overrides too"
CODE=$(req GET "/repos?tag=e2e-manual"); expect 200 "$CODE" "GET /repos?tag="
assert_eq 1 "$(jget 'length')" "tag filter matched the override-only tag"
assert_eq "$NAME" "$(jget '.[0].name')" "tag-filtered repo name"

CODE=$(req GET "/repos?tag=no-such-tag"); expect 200 "$CODE" "GET /repos?tag=no-such-tag"
assert_eq 0 "$(jget 'length')" "unknown tag matches nothing"

step "POST /repos — upsert by name updates in place rather than duplicating"
# POST /repos is REPLACE semantics, not merge: `name` is the upsert key and every
# other column is overwritten from the body, so a field you omit is written back
# as its zero value. Send the whole object, the way a real client holding a Repo
# would. (The partial-body footgun is pinned separately, below.)
CODE=$(req POST /repos \
  "{\"name\":\"$NAME\",\"path\":\"$FIXTURE\",\"description\":\"updated by e2e-smoke\",\"tag_overrides\":[\"e2e-manual\"]}")
[ "$CODE" = 200 ] || [ "$CODE" = 201 ] || fail "re-POST /repos: expected 200/201, got $CODE"
CODE=$(req GET /repos); expect 200 "$CODE" "GET /repos after upsert"
assert_eq 1 "$(jget 'length')" "upsert by name must not create a second row"
CODE=$(req GET "/repos/by-name/$NAME"); expect 200 "$CODE" "GET after upsert"
assert_eq "updated by e2e-smoke" "$(jget '.description')" "upsert updated the description"
assert_eq "$FIXTURE" "$(jget '.path')" "upsert preserved the path we re-sent"

step "POST /repos/{id}/detect — re-detect refreshes tags from disk"
CODE=$(req POST "/repos/$REPO_ID/detect" '')
expect 200 "$CODE" "POST /repos/{id}/detect"
assert_has_tag '.tags[]' go "re-detect still finds the go.mod"

step "REPLACE-semantics pin — a partial upsert body ZEROES the fields it omits"
# Worth knowing before you reach for POST /repos as a patch route: re-upserting
# with only {name, description} does not merge, it rewrites the row — and `path`
# comes back empty, which then makes POST /repos/{id}/detect 400 ("repo has no
# path to detect from"). That is consistent upsert behavior, not a bug, but it
# is a sharp edge and this pins it so it cannot change silently.
CODE=$(req POST /repos "{\"name\":\"$NAME\",\"description\":\"partial upsert\"}")
[ "$CODE" = 200 ] || [ "$CODE" = 201 ] || fail "partial re-POST /repos: expected 200/201, got $CODE"
CODE=$(req GET "/repos/by-name/$NAME"); expect 200 "$CODE" "GET after partial upsert"
assert_eq "" "$(jget '.path // ""')" "the omitted path was zeroed by the upsert"
CODE=$(req POST "/repos/$REPO_ID/detect" '')
expect 400 "$CODE" "detect on a path-less repo fails loudly rather than guessing"

# put the path back so the rest of the run has a sane row
CODE=$(req POST /repos "{\"name\":\"$NAME\",\"path\":\"$FIXTURE\",\"tag_overrides\":[\"e2e-manual\"]}")
[ "$CODE" = 200 ] || [ "$CODE" = 201 ] || fail "restore re-POST /repos: expected 200/201, got $CODE"

step "POST /repos — name is required"
CODE=$(req POST /repos '{"description":"nameless"}')
expect 400 "$CODE" "POST /repos with no name"

step "GET /repos/by-name/{name} — unknown name is a 404, not a panic"
CODE=$(req GET /repos/by-name/no-such-repo); expect 404 "$CODE" "GET unknown repo by name"

# ============================================================================
step "DELETE /repos/{id} — and it is really gone"
CODE=$(req DELETE "/repos/$REPO_ID")
[ "$CODE" = 200 ] || [ "$CODE" = 204 ] || fail "DELETE /repos/{id}: expected 200/204, got $CODE"
CODE=$(req GET "/repos/$REPO_ID"); expect 404 "$CODE" "GET deleted repo"
CODE=$(req GET /repos); expect 200 "$CODE" "GET /repos after delete"
assert_eq 0 "$(jget 'length')" "repo count after delete"

step "the server is still alive after all of that"
CODE=$(req GET /health); expect 200 "$CODE" "/health at end of run"

# ============================================================================
step "hermeticity: the live db was never opened"
if [ -e "$TMP_DIR/fake-home/.config/repo-store" ]; then
  fail "repo-store wrote to \$HOME/.config/repo-store despite REPO_STORE_DATA_DIR — the env var is being ignored"
fi
[ -f "$DATA_DIR/repo-store.db" ] || fail "the throwaway db was never created at $DATA_DIR/repo-store.db"
echo "    only $DATA_DIR/repo-store.db was written"

step "SUCCESS — repo-store boots and answers from this tree"
echo "    server log: $TMP_DIR/server.log"
