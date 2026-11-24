#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="people-store.service"
BINARY="people-store"
# Commands an operator or a scheduler job runs, built from cmd/<name> and
# installed as ~/bin/<name>.
COMMANDS=(people-import-multichat people-import-contacts)
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"

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

echo "==> Building $BINARY (tags: $GO_TAGS)..."
go build -tags "$GO_TAGS" -o "$BINARY" ./cmd/people-store
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"
for name in "${COMMANDS[@]}"; do
  go build -tags "$GO_TAGS" -o "$name" "./cmd/$name"
  echo "    built: $name"
done

# A set PEOPLE_STORE_ variable that settings.go does not declare stops the new
# binary at boot. Ask before the old one is stopped: build the registry from the
# running service's own environment. The test prints a verdict, never a value.
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
for name in "${COMMANDS[@]}"; do
  install -m 755 "$name" "$BIN_DIR/$name"
  echo "    installed: $BIN_DIR/$name"
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
# missing from a binary that otherwise starts, so prove the search path answers.
# The address is read from the unit just installed, not assumed.
echo "==> Smoke-checking the API..."
ADDR="$(sed -n 's/^Environment=PEOPLE_STORE_ADDR=//p' "$UNIT_SRC")"
if [ -z "$ADDR" ]; then
  echo "ERROR: $UNIT_SRC sets no PEOPLE_STORE_ADDR"
  exit 1
fi
BASE="http://${ADDR}"
curl -sfS "$BASE/health" >/dev/null || { echo "ERROR: /health did not answer"; exit 1; }
curl -sfS "$BASE/persons?q=test" >/dev/null || {
  echo "ERROR: full-text search did not answer — was this built with -tags $GO_TAGS?"
  exit 1
}
curl -sfS "$BASE/settings" >/dev/null || { echo "ERROR: /settings did not answer"; exit 1; }
echo "    /health, full-text search and /settings all answered"

# The bind is part of the contract: this service has no auth and must not be
# reachable off-host. Fail the deploy if it is listening anywhere else.
echo "==> Checking the bind..."
if ss -tlnH "sport = :${ADDR##*:}" | grep -q '127\.0\.0\.1:'; then
  echo "    bound to 127.0.0.1"
else
  echo "ERROR: people-store is not bound to 127.0.0.1:"
  ss -tlnp "sport = :${ADDR##*:}"
  exit 1
fi

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
