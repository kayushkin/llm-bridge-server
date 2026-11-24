#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

# Mirrors skill-store/deploy.sh: builds the cmd/server binary, drops it
# into ~/bin/agent-store, and bounces the user systemd unit. Same DBus
# env shim so the script works from non-login shells (Claude, automation).

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="agent-store.service"
BINARY="agent-store"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/server
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"

echo "==> Checking the running service's environment..."
# A set AGENT_STORE_ variable the declarations do not name, or an interval that
# is not a number, stops the new binary at boot. Build the registry from the
# running service's own environment first. The test prints a verdict, never a
# value.
LIVE_PID="$(systemctl --user show -p MainPID --value "$SERVICE")"
if [ -n "$LIVE_PID" ] && [ "$LIVE_PID" != "0" ]; then
  go test -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' ./internal/config -args -live-environment-file="/proc/$LIVE_PID/environ" \
    || { echo "ERROR: the new binary would refuse the running service's environment — not installing"; exit 1; }
else
  echo "    $SERVICE is not running, so there is no environment to check"
fi

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binary to $BIN_DIR..."
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"

echo "==> Starting $SERVICE..."
systemctl --user daemon-reload
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

echo "==> Smoke test..."
# Health lives at /health (not /agents/health): this process owns its mux, so
# cmd/server registers it via RegisterHealthHandler. The library deliberately
# does not -- a generic route there panics hosts that embed agent-store.
if ! curl -fsS http://localhost:8300/health >/dev/null 2>&1; then
  echo "ERROR: $SERVICE not responding on :8300/health"
  journalctl --user -u "$SERVICE" -n 30 --no-pager 2>&1
  exit 1
fi
echo "    smoke test OK"

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
