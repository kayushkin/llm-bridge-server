#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="budget-store.service"
BINARY="budget-store"
# Commands an operator or a scheduler job runs, built from cmd/<name> and
# installed as ~/bin/<name>.
COMMANDS=(budget-suggest-categories)
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test ./...

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/budget-store
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"
for name in "${COMMANDS[@]}"; do
  go build -o "$name" "./cmd/$name"
  echo "    built: $name"
done

# A set BUDGET_STORE_ variable that settings.go does not declare stops the new
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

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing $BINARY to $BIN_DIR..."
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
if ! systemctl --user is-active --quiet "$SERVICE"; then
  echo "ERROR: $SERVICE failed to start"
  journalctl --user -u "$SERVICE" -n 20 --no-pager 2>&1
  exit 1
fi
echo "    $SERVICE is running"

# A running process is not a working one: prove the API reads the database.
echo "==> Smoke-checking the API..."
ADDR="$(sed -n 's/^Environment=BUDGET_STORE_ADDR=//p' "$UNIT_SRC")"
if [ -z "$ADDR" ]; then
  echo "ERROR: $UNIT_SRC sets no BUDGET_STORE_ADDR"
  exit 1
fi
BASE="http://${ADDR}"
for route in /health /accounts /categories /settings; do
  curl -sfS "$BASE$route" >/dev/null || { echo "ERROR: $route did not answer"; exit 1; }
done
echo "    /health, /accounts, /categories and /settings answered"

# The bind is part of the contract: no auth, so never reachable off-host.
echo "==> Checking the bind..."
if ss -tlnH "sport = :${ADDR##*:}" | grep -q '127\.0\.0\.1:'; then
  echo "    bound to 127.0.0.1"
else
  echo "ERROR: budget-store is not bound to 127.0.0.1:"
  ss -tlnp "sport = :${ADDR##*:}"
  exit 1
fi

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
