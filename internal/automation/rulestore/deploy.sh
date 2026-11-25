#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="rule-store.service"
BINARY="rule-store"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test ./...

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/rule-store

# A set RULE_STORE_ variable that settings.go does not declare stops the new
# binary at boot. Ask before the old one is stopped. The test prints a verdict,
# never a value.
echo "==> Checking the running service's environment against the declared settings..."
live_pid="$(systemctl --user show -p MainPID --value "$SERVICE" 2>/dev/null || true)"
if [ -n "$live_pid" ] && [ "$live_pid" != "0" ]; then
  go test -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' . -args -live-environment-file="/proc/$live_pid/environ"
else
  echo "    $SERVICE is not running, so there is no environment to check"
fi

echo "==> Installing systemd unit..."
mkdir -p "$(dirname "$UNIT_DEST")"
cp "$UNIT_SRC" "$UNIT_DEST"

# Stopping the service loses nothing: rules, runs and the session copies are
# in the database, and the new process reads every session again on start.
echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binary to $BIN_DIR..."
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"

echo "==> Starting $SERVICE..."
systemctl --user daemon-reload
systemctl --user enable "$SERVICE" >/dev/null
systemctl --user start "$SERVICE"

echo "==> Verifying..."
sleep 2
if ! systemctl --user is-active --quiet "$SERVICE"; then
  echo "ERROR: $SERVICE failed to start"
  journalctl --user -u "$SERVICE" -n 20 --no-pager 2>&1
  exit 1
fi
echo "    $SERVICE is running"

# The address is the unit's; read it there rather than restating the port.
ADDR="$(systemctl --user show -p Environment --value "$SERVICE" | tr ' ' '\n' | sed -n 's/^RULE_STORE_ADDR=//p')"
if [ -z "$ADDR" ]; then
  echo "ERROR: $SERVICE sets no RULE_STORE_ADDR"
  exit 1
fi
BASE="http://${ADDR/#:/localhost:}"
echo "==> Smoke-checking $BASE..."
curl -sfS "$BASE/health" >/dev/null || { echo "ERROR: /health did not answer"; exit 1; }
curl -sfS "$BASE/settings" >/dev/null || { echo "ERROR: /settings did not answer"; exit 1; }
echo "    /health and /settings answered"

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
