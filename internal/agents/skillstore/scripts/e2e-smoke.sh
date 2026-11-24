#!/usr/bin/env bash
# Boot-and-answer smoke test for skill-store.
#
# Builds ./cmd/skill-store from THIS checkout, launches it against a throwaway
# data dir on a throwaway port, and drives the full local lifecycle over HTTP:
# register a source → sync it → list/filter/read skills → read a payload file →
# install → seed manifest/version/drift → uninstall → re-sync (idempotence) →
# prune → delete. Every assertion looks at parsed response bodies, not just
# exit codes.
#
# Why this exists: `go build` proves the tree COMPILES, not that the binary
# BOOTS. Go 1.22+ http.ServeMux panics at *registration* time on a conflicting
# route pattern, and no compiler sees that. This script is Tier 2 of the nightly
# repo-build guard (healthcheck/scripts/repo-build-audit.sh): it runs from a
# clean clone of the committed HEAD and proves the binary starts, opens its DB,
# registers its routes, and answers correctly.
#
# NO NETWORK. skill-store ingests skills by `git clone`ing a source repo, so
# the smoke builds a real git repo in the temp dir and registers THAT as the
# source's repo_url. syncRepo() (ingest.go) shells out to plain `git clone` /
# `git fetch`, which is transport-agnostic — a local path exercises the exact
# same ingest code path as github.com, with zero packets. Nothing here resolves
# a hostname; a nightly job must not depend on GitHub being up.
#
# NO LIVE STATE. The live service uses port 8301, ~/.config/skill-store (data)
# and ~/.claude/skills (install target). This run uses none of them:
#   * SKILL_STORE_ADDR         → 127.0.0.1:$E2E_PORT
#   * SKILL_STORE_DATA_DIR     → $TMP_DIR/data
#   * SKILL_STORE_INSTALL_DIR  → $TMP_DIR/home/custom-skills
#   * HOME                     → $TMP_DIR/home
# The install dir is what makes the install/uninstall/seed half of this test
# safe to run at all: without an override the server copies skills straight into
# the live ~/.claude/skills. It is deliberately NOT $FAKE_HOME/.claude/skills —
# pointing it somewhere the default could never produce is what proves the env
# var is actually honored rather than coincidentally agreeing with the default,
# and the run asserts $FAKE_HOME/.claude is never created.
#
# HOME is still redirected, for two reasons: BuildSeedManifest rewrites
# install_path relative to $HOME (so the install dir must stay *under* it, which
# is why the custom dir lives inside $FAKE_HOME), and it is belt-and-braces
# against any future $HOME-relative default.
#
# No credentials: the server reads exactly three env vars (SKILL_STORE_ADDR,
# SKILL_STORE_DATA_DIR, SKILL_STORE_INSTALL_DIR) and no token — verified in
# cmd/skill-store/main.go. The only hardcoded github.com URLs in the repo live
# in cmd/seed/main.go, a separate binary this script never builds or runs.
#
# Exits 0 on success, non-zero on the FIRST failing assertion, dumping the
# server log to stderr.
#
# Tunables:
#   E2E_PORT — listen port (default 19108)
#   E2E_KEEP — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19108}"
BASE="http://127.0.0.1:$PORT"

for bin in go curl jq git; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t skill-store-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
FAKE_HOME="$TMP_DIR/home"
# Under $FAKE_HOME (BuildSeedManifest skips installs outside $HOME) but NOT at
# the default .claude/skills — so the install assertions below can only pass if
# SKILL_STORE_INSTALL_DIR is honored.
INSTALL_DIR="$FAKE_HOME/custom-skills"
FIXTURE_REPO="$TMP_DIR/fixture-repo"
SERVER_LOG="$TMP_DIR/server.log"
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
fail() {
  echo "FAIL: $*" >&2
  echo "----- server.log -----" >&2
  cat "$SERVER_LOG" >&2 2>/dev/null || true
  exit 1
}
# eq <got> <want> <what>
eq() { [ "$1" = "$2" ] || fail "$3: got '$1', want '$2'"; }

# GET/POST helpers that surface the status code instead of curl's opaque exit.
# `curl -f` is deliberately NOT used where we assert on a non-2xx code.
code_of() { # code_of <method> <path> [curl args...]
  local method="$1" path="$2"; shift 2
  curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X "$method" "$BASE$path" "$@"
}

step "build ./cmd/skill-store from $REPO_DIR"
cd "$REPO_DIR"
go build -o "$BIN_DIR/skill-store" ./cmd/skill-store
echo "    binary: $(ls -lh "$BIN_DIR/skill-store" | awk '{print $5}')"

step "build hermetic fixture git repo (no network)"
# Two skills so list/filter/prune assertions have something to discriminate.
# e2e-hello carries two sibling payload files; e2e-widget carries none.
mkdir -p "$FIXTURE_REPO/skills/e2e-hello/scripts" "$FIXTURE_REPO/skills/e2e-widget"
cat >"$FIXTURE_REPO/skills/e2e-hello/SKILL.md" <<'SKILLMD'
---
name: e2e-hello
display_name: E2E Hello
description: Hermetic fixture skill for the skill-store smoke test.
tags: [e2e, fixture]
---

# E2E Hello

E2E_SKILL_BODY_MARKER
SKILLMD
printf '#!/bin/sh\necho E2E_PAYLOAD_MARKER\n' >"$FIXTURE_REPO/skills/e2e-hello/scripts/run.sh"
printf '# reference\nE2E_REFERENCE_MARKER\n' >"$FIXTURE_REPO/skills/e2e-hello/reference.md"
cat >"$FIXTURE_REPO/skills/e2e-widget/SKILL.md" <<'SKILLMD'
---
name: e2e-widget
description: Second fixture skill, exists to be filtered out and pruned.
tags: [e2e]
---

# E2E Widget
SKILLMD
# A sibling payload file, so e2e-widget gets a dir under $DATA_DIR/skills. That
# dir is what pruneOrphanSkillDirs must reap when the skill disappears upstream;
# without a payload file the prune step has nothing to reap and never runs.
printf '# widget notes\nE2E_WIDGET_MARKER\n' >"$FIXTURE_REPO/skills/e2e-widget/notes.md"
# A file outside the subpath — sync must ignore it (subpath scoping).
printf 'not a skill\n' >"$FIXTURE_REPO/README.md"

GIT_Q=(git -c user.email=e2e@localhost -c user.name=e2e-smoke -c commit.gpgsign=false)
git -C "$FIXTURE_REPO" init -q -b main
"${GIT_Q[@]}" -C "$FIXTURE_REPO" add -A
"${GIT_Q[@]}" -C "$FIXTURE_REPO" commit -q -m 'fixture: two skills'
FIXTURE_COMMIT="$(git -C "$FIXTURE_REPO" rev-parse HEAD)"
echo "    fixture repo: $FIXTURE_REPO @ ${FIXTURE_COMMIT:0:12}"

step "launch skill-store on 127.0.0.1:$PORT"
echo "    SKILL_STORE_DATA_DIR=$DATA_DIR"
echo "    SKILL_STORE_INSTALL_DIR=$INSTALL_DIR  (keeps installs out of ~/.claude/skills)"
echo "    HOME=$FAKE_HOME"
SKILL_STORE_ADDR="127.0.0.1:$PORT" \
SKILL_STORE_DATA_DIR="$DATA_DIR" \
SKILL_STORE_INSTALL_DIR="$INSTALL_DIR" \
HOME="$FAKE_HOME" \
  "$BIN_DIR/skill-store" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID"

# Poll for readiness — bounded, never a bare sleep-and-hope.
READY=""
for _ in $(seq 1 60); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    fail "server process exited during startup (route-registration panic?)"
  fi
  if curl -fsS --max-time 2 "$BASE/health" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.2
done
[ -n "$READY" ] || fail "server did not answer $BASE/health within 12s"

step "GET /health"
HEALTH="$(curl -fsS --max-time 10 "$BASE/health")"
eq "$(jq -r '.status' <<<"$HEALTH")" "ok" "/health .status"
echo "    $HEALTH"

step "empty store answers well-formed empty bodies"
# These prove the routes are registered and the DB opened, independent of ingest.
for path in /skills /sources /installations; do
  BODY="$(curl -fsS --max-time 10 "$BASE$path")"
  eq "$BODY" "[]" "GET $path on an empty store"
  echo "    GET $path -> []"
done

step "POST /sources (repo_url = local fixture path, no network)"
SRC="$(curl -fsS --max-time 10 -X POST "$BASE/sources" \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg url "$FIXTURE_REPO" \
        '{name:"e2e-local", repo_url:$url, branch:"main", subpath:"skills", trust_tier:"personal", license:"MIT"}')")"
SRC_ID="$(jq -r '.id' <<<"$SRC")"
[ -n "$SRC_ID" ] && [ "$SRC_ID" != "null" ] && [ "$SRC_ID" -gt 0 ] || fail "POST /sources returned no id: $SRC"
eq "$(jq -r '.name'       <<<"$SRC")" "e2e-local" "source .name"
eq "$(jq -r '.branch'     <<<"$SRC")" "main"      "source .branch"
eq "$(jq -r '.trust_tier' <<<"$SRC")" "personal"  "source .trust_tier"
eq "$(jq -r '.enabled'    <<<"$SRC")" "true"      "source .enabled (createSource forces true)"
echo "    source id: $SRC_ID"

eq "$(code_of POST /sources -H 'Content-Type: application/json' -d '{"name":"no-url"}')" \
   "400" "POST /sources without repo_url"
eq "$(code_of POST /sources/99999999/sync)" "404" "POST /sources/<missing>/sync"
eq "$(code_of POST /sources/not-a-number/sync)" "400" "POST /sources/<non-numeric>/sync"

step "POST /sources/$SRC_ID/sync — ingest from the local git repo"
SYNC="$(curl -fsS --max-time 60 -X POST "$BASE/sources/$SRC_ID/sync")"
echo "    $SYNC"
eq "$(jq -r '.found'    <<<"$SYNC")" "2" "sync .found (two SKILL.md under skills/)"
eq "$(jq -r '.inserted' <<<"$SYNC")" "2" "sync .inserted"
eq "$(jq -r '.updated'  <<<"$SYNC")" "0" "sync .updated"
eq "$(jq -r '.pruned'   <<<"$SYNC")" "0" "sync .pruned"
eq "$(jq -r '.commit'   <<<"$SYNC")" "$FIXTURE_COMMIT" "sync .commit == fixture HEAD"
eq "$(jq -r '.errors // [] | length' <<<"$SYNC")" "0" "sync reported errors"

step "GET /skills — listing, filters, frontmatter parse"
SKILLS="$(curl -fsS --max-time 10 "$BASE/skills")"
eq "$(jq -r 'length' <<<"$SKILLS")" "2" "GET /skills count"
eq "$(jq -r '[.[].name] | join(",")' <<<"$SKILLS")" "e2e-hello,e2e-widget" "GET /skills names (ordered)"
eq "$(jq -r '.[0].source_name' <<<"$SKILLS")" "e2e-local" "skill .source_name (JOIN on sources)"

eq "$(curl -fsS --max-time 10 "$BASE/skills?q=widget"       | jq -r '[.[].name]|join(",")')" "e2e-widget" "?q= substring filter"
eq "$(curl -fsS --max-time 10 "$BASE/skills?tag=fixture"    | jq -r '[.[].name]|join(",")')" "e2e-hello"  "?tag= filter"
eq "$(curl -fsS --max-time 10 "$BASE/skills?tag=e2e"        | jq -r 'length')"               "2"          "?tag= filter (both)"
eq "$(curl -fsS --max-time 10 "$BASE/skills?source=e2e-local" | jq -r 'length')"             "2"          "?source= filter"
eq "$(curl -fsS --max-time 10 "$BASE/skills?limit=1"        | jq -r 'length')"               "1"          "?limit= bound"
eq "$(curl -fsS --max-time 10 "$BASE/skills?source=nope"    | jq -r 'length')"               "0"          "?source= miss returns empty"
eq "$(code_of GET '/skills?limit=abc')" "400" "?limit=abc rejected"

HELLO_ID="$(jq -r '.[] | select(.name=="e2e-hello") | .id' <<<"$SKILLS")"
WIDGET_ID="$(jq -r '.[] | select(.name=="e2e-widget") | .id' <<<"$SKILLS")"
[ -n "$HELLO_ID" ] && [ "$HELLO_ID" != "null" ] || fail "no id for e2e-hello"
echo "    e2e-hello id=$HELLO_ID  e2e-widget id=$WIDGET_ID"

step "GET /skills/$HELLO_ID — full record"
SK="$(curl -fsS --max-time 10 "$BASE/skills/$HELLO_ID")"
eq "$(jq -r '.name'         <<<"$SK")" "e2e-hello"  "skill .name"
eq "$(jq -r '.display_name' <<<"$SK")" "E2E Hello"  "skill .display_name (frontmatter)"
eq "$(jq -r '.description'  <<<"$SK")" "Hermetic fixture skill for the skill-store smoke test." "skill .description"
eq "$(jq -r '.tags | join(",")' <<<"$SK")" "e2e,fixture" "skill .tags (YAML flow seq → JSON array)"
eq "$(jq -r '.source_path'   <<<"$SK")" "skills/e2e-hello" "skill .source_path (relative to repo root)"
eq "$(jq -r '.source_commit' <<<"$SK")" "$FIXTURE_COMMIT"  "skill .source_commit"
eq "$(jq -r '.file_count'    <<<"$SK")" "2" "skill .file_count (siblings, SKILL.md excluded)"
eq "$(jq -r '.enabled'       <<<"$SK")" "true" "skill .enabled"
eq "$(jq -r '.installed'     <<<"$SK")" "false" "skill .installed (nothing installed yet)"
jq -r '.skill_md' <<<"$SK" | grep -q 'E2E_SKILL_BODY_MARKER' \
  || fail "skill .skill_md did not contain the fixture body marker"
# content_hash must be the sha256 of the stored SKILL.md, not a placeholder.
eq "$(jq -r '.content_hash' <<<"$SK")" \
   "$(sha256sum "$FIXTURE_REPO/skills/e2e-hello/SKILL.md" | awk '{print $1}')" \
   "skill .content_hash == sha256(SKILL.md)"

eq "$(code_of GET /skills/99999999)" "404" "GET /skills/<missing>"
eq "$(code_of GET /skills/not-a-number)" "400" "GET /skills/<non-numeric>"

step "GET /skills/$HELLO_ID/files + /files/{path...}"
FILES="$(curl -fsS --max-time 10 "$BASE/skills/$HELLO_ID/files")"
echo "    $(jq -c '[.[].path]' <<<"$FILES")"
eq "$(jq -r 'length' <<<"$FILES")" "2" "/files count"
eq "$(jq -r '[.[].path] | sort | join(",")' <<<"$FILES")" "reference.md,scripts/run.sh" "/files paths"
eq "$(jq -r '.[] | select(.path=="scripts/run.sh") | .size > 0' <<<"$FILES")" "true" "/files reports a real size"
# SKILL.md lives in the DB, not the payload dir — it must NOT appear here.
eq "$(jq -r '[.[] | select(.path=="SKILL.md")] | length' <<<"$FILES")" "0" "/files must not list SKILL.md"

# The {path...} wildcard route: a nested path is exactly the shape that would
# collide with GET /skills/{id}/files and panic the mux at registration.
BODY="$(curl -fsS --max-time 10 "$BASE/skills/$HELLO_ID/files/scripts/run.sh")"
grep -q 'E2E_PAYLOAD_MARKER' <<<"$BODY" || fail "/files/scripts/run.sh did not serve the payload: $BODY"
echo "    served scripts/run.sh ($(printf '%s' "$BODY" | wc -c) bytes)"
eq "$(code_of GET "/skills/$HELLO_ID/files/nope.txt")" "404" "/files/<missing>"

# Path traversal must not serve anything outside the skill dir. Go's ServeMux
# cleans dot segments before routing (301), and the handler rejects the rest;
# either way the only acceptable outcome is "not 200, no leak".
TRAV="$TMP_DIR/traversal.out"
TCODE="$(curl -s --path-as-is -o "$TRAV" -w '%{http_code}' --max-time 10 \
  "$BASE/skills/$HELLO_ID/files/../../../../../../etc/passwd")"
[ "$TCODE" != "200" ] || fail "path traversal returned 200 (code=$TCODE)"
grep -q 'root:' "$TRAV" && fail "path traversal leaked /etc/passwd" || true
echo "    traversal blocked (HTTP $TCODE, no leak)"

step "POST /skills/$HELLO_ID/install — into SKILL_STORE_INSTALL_DIR=$INSTALL_DIR"
INST="$(curl -fsS --max-time 10 -X POST "$BASE/skills/$HELLO_ID/install")"
TARGET="$(jq -r '.target_path' <<<"$INST")"
# The load-bearing assertion of this whole file: the install landed where the
# env var said, not at DefaultInstallDir(). A build that ignores
# SKILL_STORE_INSTALL_DIR writes to $FAKE_HOME/.claude/skills and fails here.
eq "$TARGET" "$INSTALL_DIR/e2e-hello" "install .target_path == SKILL_STORE_INSTALL_DIR/<name>"
[ ! -e "$FAKE_HOME/.claude" ] || fail "install fell back to the default ~/.claude/skills despite SKILL_STORE_INSTALL_DIR"
eq "$(jq -r '.skill_name' <<<"$INST")" "e2e-hello" "install .skill_name"
# Assert on the filesystem, not just the JSON: the payload really landed.
[ -f "$TARGET/SKILL.md" ] || fail "install did not write $TARGET/SKILL.md"
grep -q 'E2E_SKILL_BODY_MARKER' "$TARGET/SKILL.md" || fail "installed SKILL.md has wrong content"
grep -q 'E2E_PAYLOAD_MARKER' "$TARGET/scripts/run.sh" || fail "install did not copy sibling scripts/run.sh"
grep -q 'E2E_REFERENCE_MARKER' "$TARGET/reference.md" || fail "install did not copy sibling reference.md"
echo "    installed to $TARGET (SKILL.md + 2 payload files)"

eq "$(curl -fsS --max-time 10 "$BASE/installations" | jq -r 'length')" "1" "GET /installations after install"
eq "$(curl -fsS --max-time 10 "$BASE/installations" | jq -r '.[0].skill_name')" "e2e-hello" "installation .skill_name"
eq "$(curl -fsS --max-time 10 "$BASE/skills?installed=true" | jq -r '[.[].name]|join(",")')" "e2e-hello" "?installed=true filter"
eq "$(curl -fsS --max-time 10 "$BASE/skills/$HELLO_ID" | jq -r '.installed')" "true" "skill .installed flips after install"
# Installing a *different* skill over the same target is a 409 without ?force.
# (e2e-widget has a different name, so it cannot collide — assert the happy
# path of the conflict guard instead: re-installing the same skill is a no-op.)
eq "$(code_of POST "/skills/$HELLO_ID/install")" "200" "re-install of the same skill is idempotent"

step "seed endpoints: profile → manifest → version content → drift"
PROF="$(curl -fsS --max-time 10 "$BASE/seed/profile?machine_id=e2e-machine")"
eq "$(jq -r '.machine_id' <<<"$PROF")" "e2e-machine" "seed profile .machine_id"
eq "$(jq -r '.enabled'    <<<"$PROF")" "true"        "seed profile defaults to enabled"
eq "$(code_of GET /seed/profile)" "400" "GET /seed/profile without machine_id"

MF="$(curl -fsS --max-time 10 "$BASE/seed/manifest?machine_id=e2e-machine")"
eq "$(jq -r '.enabled' <<<"$MF")" "true" "manifest .enabled"
eq "$(jq -r '.entries | length' <<<"$MF")" "3" "manifest entry count (SKILL.md + 2 payload files)"
eq "$(jq -r '[.entries[].relpath] | sort | join(",")' <<<"$MF")" "SKILL.md,reference.md,scripts/run.sh" "manifest relpaths"
# install_path is rewritten $HOME-relative — with HOME faked, it must be the
# fake-home-relative path and must NOT be absolute.
eq "$(jq -r '.entries[] | select(.relpath=="SKILL.md") | .install_path' <<<"$MF")" \
   "custom-skills/e2e-hello/SKILL.md" "manifest .install_path (\$HOME-relative)"
VID="$(jq -r '.entries[] | select(.relpath=="SKILL.md") | .version_id' <<<"$MF")"
[ "$VID" -gt 0 ] 2>/dev/null || fail "manifest .version_id not a positive id: $VID"
MF_SHA="$(jq -r '.entries[] | select(.relpath=="SKILL.md") | .sha256' <<<"$MF")"
eq "$MF_SHA" "$(sha256sum "$TARGET/SKILL.md" | awk '{print $1}')" "manifest .sha256 == sha256 of the installed file"

VC="$(curl -fsS --max-time 10 "$BASE/versions/$VID/content")"
eq "$(jq -r '.version.relpath' <<<"$VC")" "SKILL.md" "version .relpath"
eq "$(jq -r '.version.source'  <<<"$VC")" "ingest"   "version .source"
eq "$(jq -r '.version.sha256'  <<<"$VC")" "$MF_SHA"  "version .sha256"
jq -r '.content' <<<"$VC" | grep -q 'E2E_SKILL_BODY_MARKER' || fail "/versions/$VID/content returned wrong bytes"
eq "$(code_of GET /versions/99999999/content)" "404" "GET /versions/<missing>/content"

# A runner reporting drifted bytes must append a NEW version with a new sha.
DRIFT="$(curl -fsS --max-time 10 -X POST "$BASE/seed/drift" \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --argjson sid "$HELLO_ID" \
        '{machine_id:"e2e-machine", skill_id:$sid, relpath:"SKILL.md",
          content:"---\nname: e2e-hello\n---\nE2E_DRIFTED_BODY\n", note:"smoke drift"}')")"
DRIFT_VID="$(jq -r '.id' <<<"$DRIFT")"
eq "$(jq -r '.source' <<<"$DRIFT")" "runner-drift" "drift version .source"
eq "$(jq -r '.machine_id' <<<"$DRIFT")" "e2e-machine" "drift version .machine_id"
[ "$DRIFT_VID" != "$VID" ] || fail "drift did not append a new version (id still $VID)"
[ "$(jq -r '.sha256' <<<"$DRIFT")" != "$MF_SHA" ] || fail "drift version reused the canonical sha"
DC="$(curl -fsS --max-time 10 "$BASE/versions/$DRIFT_VID/content" | jq -r '.content')"
grep -q 'E2E_DRIFTED_BODY' <<<"$DC" || fail "drift content did not round-trip"
echo "    drift version $DRIFT_VID appended (canonical was $VID)"

eq "$(code_of POST /seed/observe -H 'Content-Type: application/json' -d '{"machine_id":"m"}')" \
   "400" "POST /seed/observe without skill_id/relpath"
eq "$(code_of POST /seed/observe -H 'Content-Type: application/json' \
      -d "{\"machine_id\":\"e2e-machine\",\"skill_id\":$HELLO_ID,\"relpath\":\"SKILL.md\",\"sha256\":\"$MF_SHA\"}")" \
   "200" "POST /seed/observe records an observed hash"

# Opting the machine out must empty its manifest.
OFF="$(curl -fsS --max-time 10 -X PUT "$BASE/seed/profile?machine_id=e2e-machine" \
  -H 'Content-Type: application/json' -d '{"enabled":false}')"
eq "$(jq -r '.enabled' <<<"$OFF")" "false" "PUT /seed/profile enabled=false"
MF_OFF="$(curl -fsS --max-time 10 "$BASE/seed/manifest?machine_id=e2e-machine")"
eq "$(jq -r '.enabled' <<<"$MF_OFF")" "false" "manifest .enabled after opt-out"
eq "$(jq -r '.entries | length' <<<"$MF_OFF")" "0" "manifest is empty for an opted-out machine"

step "POST /skills/$HELLO_ID/uninstall"
UN="$(curl -fsS --max-time 10 -X POST "$BASE/skills/$HELLO_ID/uninstall")"
eq "$(jq -r '.target_path' <<<"$UN")" "$TARGET" "uninstall .target_path"
[ ! -e "$TARGET" ] || fail "uninstall left $TARGET on disk"
eq "$(curl -fsS --max-time 10 "$BASE/installations")" "[]" "GET /installations after uninstall"
eq "$(curl -fsS --max-time 10 "$BASE/skills/$HELLO_ID" | jq -r '.installed')" "false" "skill .installed flips back"
eq "$(curl -fsS --max-time 10 -X POST "$BASE/skills/$HELLO_ID/uninstall" | jq -r '.status')" \
   "not-installed" "uninstalling twice is a clean no-op"

step "re-sync is idempotent (git fetch path, still local)"
SYNC2="$(curl -fsS --max-time 60 -X POST "$BASE/sources/$SRC_ID/sync")"
echo "    $SYNC2"
eq "$(jq -r '.found'    <<<"$SYNC2")" "2" "re-sync .found"
eq "$(jq -r '.inserted' <<<"$SYNC2")" "0" "re-sync must insert nothing"
eq "$(jq -r '.updated'  <<<"$SYNC2")" "2" "re-sync .updated"
eq "$(jq -r '.pruned'   <<<"$SYNC2")" "0" "re-sync must prune nothing"
eq "$(curl -fsS --max-time 10 "$BASE/skills" | jq -r '[.[].id]|sort|map(tostring)|join(",")')" \
   "$(printf '%s\n%s\n' "$HELLO_ID" "$WIDGET_ID" | sort -n | paste -sd,)" \
   "re-sync must not renumber skill ids"

step "prune: drop a skill upstream, re-sync"
rm -rf "$FIXTURE_REPO/skills/e2e-widget"
"${GIT_Q[@]}" -C "$FIXTURE_REPO" add -A
"${GIT_Q[@]}" -C "$FIXTURE_REPO" commit -q -m 'fixture: remove e2e-widget'
SYNC3="$(curl -fsS --max-time 60 -X POST "$BASE/sources/$SRC_ID/sync")"
echo "    $SYNC3"
eq "$(jq -r '.found'  <<<"$SYNC3")" "1" "prune-sync .found"
eq "$(jq -r '.pruned' <<<"$SYNC3")" "1" "prune-sync .pruned"
eq "$(jq -r '.commit' <<<"$SYNC3")" "$(git -C "$FIXTURE_REPO" rev-parse HEAD)" "prune-sync .commit == new HEAD"
eq "$(curl -fsS --max-time 10 "$BASE/skills" | jq -r '[.[].name]|join(",")')" "e2e-hello" "e2e-widget is gone"
eq "$(code_of GET "/skills/$WIDGET_ID")" "404" "pruned skill is 404"
eq "$(jq -r '.errors // [] | length' <<<"$SYNC3")" "0" "prune-sync reported errors"
# The pruned skill's payload dir must be reaped too, not just its DB row.
[ ! -e "$DATA_DIR/skills/$WIDGET_ID" ] || fail "prune left the orphan payload dir $DATA_DIR/skills/$WIDGET_ID"
[ -e "$DATA_DIR/skills/$HELLO_ID" ] || fail "prune reaped the payload dir of a skill that still exists"
echo "    orphan payload dir $DATA_DIR/skills/$WIDGET_ID reaped; e2e-hello's kept"

step "DELETE /skills/$HELLO_ID and DELETE /sources/$SRC_ID"
eq "$(curl -fsS --max-time 10 -X DELETE "$BASE/skills/$HELLO_ID" | jq -r '.status')" "deleted" "DELETE /skills"
eq "$(curl -fsS --max-time 10 "$BASE/skills")" "[]" "no skills left"
[ ! -e "$DATA_DIR/skills/$HELLO_ID" ] || fail "DELETE /skills left the payload dir $DATA_DIR/skills/$HELLO_ID"
eq "$(curl -fsS --max-time 10 -X DELETE "$BASE/sources/$SRC_ID" | jq -r '.status')" "deleted" "DELETE /sources"
eq "$(curl -fsS --max-time 10 "$BASE/sources")" "[]" "no sources left"
eq "$(code_of GET "/sources/$SRC_ID")" "404" "deleted source is 404"

step "the live store was never touched"
# Belt and braces: nothing this run created may sit outside $TMP_DIR.
[ -f "$DATA_DIR/skill-store.db" ] || fail "expected a temp DB at $DATA_DIR/skill-store.db"
[ ! -e "$FAKE_HOME/.config/skill-store" ] || fail "server fell back to \$HOME/.config/skill-store despite SKILL_STORE_DATA_DIR"
[ ! -e "$FAKE_HOME/.claude" ] || fail "server fell back to \$HOME/.claude/skills despite SKILL_STORE_INSTALL_DIR"
echo "    temp DB:      $DATA_DIR/skill-store.db"
echo "    temp install: $INSTALL_DIR (live ~/.claude/skills untouched)"

step "SUCCESS — skill-store boots, opens its DB, registers its routes, and answers"
echo "    server log: $SERVER_LOG"
