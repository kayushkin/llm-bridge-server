#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="journal-store.service"
BINARY="journal-store"
# Commands an operator runs, built from cmd/<name> and installed as ~/bin/<name>.
TOOLS=(journal-import-files journal-import-facebook-posts journal-point-transcripts-at-messages journal-timeline-discord-events journal-timeline-calendar-events)
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"
DROP_IN="$HOME/.config/systemd/user/$SERVICE.d"

# schema.sql creates an FTS5 virtual table and mattn/go-sqlite3 only compiles
# FTS5 in when asked. Without this tag the build succeeds and the service dies at
# boot with "no such module: fts5".
GO_TAGS="sqlite_fts5"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test -tags "$GO_TAGS" ./...

echo "==> Building $BINARY and ${TOOLS[*]} (tags: $GO_TAGS)..."
go build -tags "$GO_TAGS" -o "$BINARY" ./cmd/journal-store
for tool in "${TOOLS[@]}"; do
  go build -tags "$GO_TAGS" -o "$tool" "./cmd/$tool"
done

# A binary with no vcs.revision cannot be traced to a commit: go build writes
# none when .git is not a directory (a worktree), and does not fail.
echo "==> Checking provenance..."
vcs_revision="$(go version -m "$BINARY" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
if [ -z "$vcs_revision" ]; then
  echo "    REFUSING TO INSTALL: no vcs.revision in $BINARY. Build from the main clone." >&2
  exit 1
fi
echo "    vcs.revision=$vcs_revision"

# A set JOURNAL_STORE_ variable that settings.go does not declare, or a missing
# required one, stops the new binary at boot. Ask before the old one is stopped:
# build the registry from the running service's own environment. The test prints
# a verdict, never a value.
echo "==> Checking the running service's environment against the declared settings..."
live_pid="$(systemctl --user show -p MainPID --value "$SERVICE" 2>/dev/null || true)"
if [ -n "$live_pid" ] && [ "$live_pid" != "0" ]; then
  go test -tags "$GO_TAGS" -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' . -args -live-environment-file="/proc/$live_pid/environ"
else
  echo "    $SERVICE is not running, so there is no environment to check"
fi

echo "==> Installing systemd unit..."
mkdir -p "$(dirname "$UNIT_DEST")"
cp "$UNIT_SRC" "$UNIT_DEST"

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binaries to $BIN_DIR..."
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"
for tool in "${TOOLS[@]}"; do
  install -m 755 "$tool" "$BIN_DIR/$tool"
  echo "    installed: $BIN_DIR/$tool"
done

echo "==> Starting $SERVICE..."
systemctl --user daemon-reload
systemctl --user enable "$SERVICE" >/dev/null
systemctl --user start "$SERVICE"

echo "==> Verifying..."
sleep 2
if systemctl --user is-active --quiet "$SERVICE"; then
  echo "    $SERVICE is running"
  journalctl --user -u "$SERVICE" -n 5 --no-pager 2>&1 | grep -v '^--' || true
else
  echo "ERROR: $SERVICE failed to start"
  journalctl --user -u "$SERVICE" -n 20 --no-pager 2>&1
  exit 1
fi

# A running process is not a working one. FTS5 is the thing most likely to be
# missing from a binary that otherwise starts, and the registry read is the one
# that a wrong token breaks, so prove both before calling the deploy done.
echo "==> Smoke-checking the API..."
ADDR="$(sed -n 's/^Environment=JOURNAL_STORE_ADDR=//p' "$UNIT_SRC")"
[ -n "$ADDR" ] || { echo "ERROR: $UNIT_SRC sets no JOURNAL_STORE_ADDR"; exit 1; }
BASE="http://${ADDR}"
health="$(curl -sfS "$BASE/health")" || { echo "ERROR: /health did not answer"; exit 1; }
curl -sfS "$BASE/entries?q=test" >/dev/null || {
  echo "ERROR: full-text search did not answer — was this built with -tags $GO_TAGS?"
  exit 1
}
if ! printf '%s' "$health" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["status"]=="ok" else 1)'; then
  echo "ERROR: /health is not ok — mentions cannot be typed until reference-store's list of types reads:"
  printf '%s\n' "$health"
  exit 1
fi
echo "    /health is ok, the entity-type registry read, and full-text search answered"

# The bind is part of the contract: this service has no auth and must not be
# reachable off-host. Fail the deploy if it is listening anywhere else.
echo "==> Checking the bind..."
if ss -tlnH "sport = :${ADDR##*:}" | grep -q '127\.0\.0\.1:'; then
  echo "    bound to 127.0.0.1"
else
  echo "ERROR: journal-store is not bound to 127.0.0.1:"
  ss -tlnp "sport = :${ADDR##*:}"
  exit 1
fi

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
