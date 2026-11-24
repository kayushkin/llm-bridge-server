#!/usr/bin/env bash
# Build, install and restart the live noteboard service, then prove it answers.
#
# The unit is installed FROM deploy/noteboard.service, and every path this
# script needs (binary, port, database) is read back OUT of that unit rather
# than restated here. There is deliberately no second copy of those values to
# drift: if the unit and this script ever disagree, it is because someone edited
# the unit, and this script follows.
#
# noteboard.service is a --user unit, so no sudo is involved.
#
# Usage: ./deploy.sh
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
UNIT_SRC="$REPO_DIR/deploy/noteboard.service"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT_NAME="noteboard.service"
BACKUP_DIR="$HOME/.local/share/noteboard-deploy-backup"

# systemctl --user needs these to reach the user manager. Without them it prints
# NOTHING and exits 0 — silence that reads like success. Set them if absent.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"

step() { printf '\n==> %s\n' "$*"; }
fail() { echo "DEPLOY FAILED: $*" >&2; exit 1; }

for bin in go curl jq systemctl; do
  command -v "$bin" >/dev/null 2>&1 || fail "required tool '$bin' not on PATH"
done
[ -f "$UNIT_SRC" ] || fail "missing unit template: $UNIT_SRC"

step "read the unit template — it is the source of truth for these values"
# Resolve systemd's %h specifier ourselves; everything below has to agree with
# what systemd will actually exec.
unit_value() { sed -n "s|^$1=||p" "$UNIT_SRC" | tail -1 | sed "s|%h|$HOME|g"; }
unit_env()   { sed -n "s|^Environment=$1=||p" "$UNIT_SRC" | tail -1 | sed "s|%h|$HOME|g"; }

BIN_PATH="$(unit_value ExecStart)"
WORK_DIR="$(unit_value WorkingDirectory)"
PORT="$(unit_env NOTEBOARD_PORT)"
DB_PATH="$(unit_env NOTEBOARD_DB)"
[ -n "$BIN_PATH" ] || fail "unit has no ExecStart"
[ -n "$PORT" ]     || fail "unit declares no NOTEBOARD_PORT — refusing to guess"
[ -n "$DB_PATH" ]  || fail "unit declares no NOTEBOARD_DB — refusing to guess (an unset one would silently open a DIFFERENT database)"
echo "    binary:   $BIN_PATH"
echo "    port:     $PORT"
echo "    database: $DB_PATH"
BASE="http://127.0.0.1:$PORT"

step "preflight"
# systemd reports a missing WorkingDirectory and a missing binary with the same
# nameless 'result: resources' failure, then crash-loops on it. Name which.
[ -d "$WORK_DIR" ] || fail "WorkingDirectory does not exist: $WORK_DIR"
[ -f "$DB_PATH" ]  || echo "    note: $DB_PATH does not exist yet — it will be created empty"
echo "    WorkingDirectory exists: $WORK_DIR"

step "build (default flags, cgo off — see ./Makefile)"
cd "$REPO_DIR"
make vet
make test
STAGED="$REPO_DIR/bin/noteboard"
make build
[ -x "$STAGED" ] || fail "make build produced no binary at $STAGED"

# Checked BEFORE the install, for the same reason the smokes are: an unidentifiable
# binary compiles perfectly and reads clean in the log, so installing first would put
# it in front of live sessions and only then tell us it cannot be traced to a commit.
echo "==> Checking provenance..."
buildinfo="$(go version -m "$STAGED")"
vcs_revision="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {{print $2}}')"
vcs_modified="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.modified$/ {{print $2}}')"
if [ -z "$vcs_revision" ]; then
    echo "    'go build' writes no VCS stamp when it cannot find a .git DIRECTORY, and it does" >&2
    echo "    not fail when that happens -- not even with -buildvcs=true. The usual cause is" >&2
    echo "    building from a git worktree, whose .git is a pointer file. Build from a real" >&2
    echo "    clone or checkout instead." >&2
    fail "refusing to install "$STAGED": no vcs.revision, so nothing ties it back to a commit"
fi
echo "    vcs.revision=$vcs_revision"
if [ "$vcs_modified" = "true" ]; then
    echo "    WARNING: built from a DIRTY tree (vcs.modified=true). $vcs_revision names the" >&2
    echo "    commit this binary was built NEAR, not the source it was built FROM, and that" >&2
    echo "    source is not recoverable from any commit. Commit first for a reproducible build." >&2
fi
echo "    built: $(ls -lh "$STAGED" | awk '{print $5}')"

step "boot-and-answer smoke on a throwaway DB, before touching the live one"
# The binary has to prove it can open a database and serve FTS5 BEFORE it gets
# installed. `go build` passing says nothing about either.
./scripts/e2e-smoke.sh >/dev/null || fail "e2e smoke failed — not installing. Run ./scripts/e2e-smoke.sh to see why."
echo "    smoke passed"

step "check the running service's environment against the declared settings"
# A misspelled NOTEBOARD_PORT or NOTEBOARD_DB, or a port that is not a number,
# stops the new binary at boot. Ask before the old one is replaced: build the
# registry from the running service's own environment. The test prints a
# verdict, never a value.
live_pid="$(systemctl --user show -p MainPID --value "$UNIT_NAME")"
if [ -n "$live_pid" ] && [ "$live_pid" != "0" ]; then
  go test -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' ./internal/settings -args -live-environment-file="/proc/$live_pid/environ" \
    || fail "the running service's environment would stop the new binary at boot — not installing"
else
  echo "    $UNIT_NAME is not running, so there is no environment to check"
fi

step "install"
mkdir -p "$BACKUP_DIR" "$(dirname "$BIN_PATH")" "$UNIT_DIR"
if [ -f "$BIN_PATH" ]; then
  cp -p "$BIN_PATH" "$BACKUP_DIR/noteboard.prev"
  echo "    previous binary backed up to $BACKUP_DIR/noteboard.prev"
fi
# cp over a running binary fails ETXTBSY. Stage alongside, then rename — which
# is atomic, so there is no window where $BIN_PATH is half-written.
cp "$STAGED" "$BIN_PATH.new"
chmod +x "$BIN_PATH.new"
mv -f "$BIN_PATH.new" "$BIN_PATH"
install -m 0644 "$UNIT_SRC" "$UNIT_DIR/$UNIT_NAME"
systemctl --user daemon-reload
echo "    installed $BIN_PATH and $UNIT_DIR/$UNIT_NAME"

step "restart $UNIT_NAME"
systemctl --user restart "$UNIT_NAME"

step "wait for it to answer on $BASE/health"
# Poll. noteboard opens SQLite and runs migrations before it binds, and that is
# not a fixed cost — a sleep long enough today is a flake tomorrow.
READY=0
for _ in $(seq 1 60); do
  if ! systemctl --user is-active --quiet "$UNIT_NAME"; then
    systemctl --user status "$UNIT_NAME" --no-pager -l | tail -20 >&2
    fail "$UNIT_NAME is not active — it died on startup (see status above)"
  fi
  if curl -fsS -o /dev/null --max-time 2 "$BASE/health" 2>/dev/null; then READY=1; break; fi
  sleep 0.25
done
[ "$READY" = "1" ] || fail "$UNIT_NAME never answered on $BASE/health within ~15s"

step "verify the live service"
HEALTH="$(curl -fsS "$BASE/health")"
echo "    /health: $HEALTH"
[ "$(jq -r '.status' <<<"$HEALTH")" = "ok" ] || fail "/health is not ok: $HEALTH"

# An empty item count means the binary opened some OTHER database — a fresh
# empty one it just created. That is the failure this assertion exists for: it
# would otherwise look like a perfectly healthy deploy.
ITEMS="$(jq -r '.items' <<<"$HEALTH")"
[ "$ITEMS" -gt 0 ] 2>/dev/null \
  || fail "/health reports $ITEMS items — the service is serving an EMPTY database, not $DB_PATH"
echo "    serving $ITEMS items from $DB_PATH"

# FTS5 at runtime. A driver without it dies in migrate() at boot, but a search
# that returns nothing would also mean a broken index — assert a real hit.
SEEDED="$(curl -fsS "$BASE/api/items?limit=1" | jq -r '.[0].id // empty')"
[ -n "$SEEDED" ] || fail "GET /api/items returned nothing on a database with $ITEMS items"
curl -fsS "$BASE/api/search?q=the" >/dev/null || fail "GET /api/search failed — FTS5 is not answering"
echo "    /api/search answered (FTS5 live)"

printf '\n==> DEPLOYED — noteboard %s is live on %s\n' "$(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null || echo '(no git)')" "$BASE"
echo "    rollback: cp $BACKUP_DIR/noteboard.prev $BIN_PATH && systemctl --user restart $UNIT_NAME"

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
