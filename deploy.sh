#!/usr/bin/env bash
set -euo pipefail

# Re-exec detached so the script survives if the calling agent's process
# is a child of llm-bridge.service (stopping the service would otherwise
# kill the shell running this script mid-deploy).
if [ -z "${DEPLOY_DETACHED:-}" ]; then
  LOG=/tmp/llm-bridge-deploy.log
  DEPLOY_DETACHED=1 setsid nohup bash "$0" "$@" </dev/null >"$LOG" 2>&1 &
  echo "detached deploy (pid=$!), tail -f $LOG"
  exit 0
fi

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_NAME="llm-bridge"
SYSTEM_BIN="/usr/local/bin/$BIN_NAME"
SERVICE="llm-bridge.service"

cd "$REPO_DIR"

# Add go to PATH if managed by mise
export PATH="$HOME/.local/share/mise/shims:$PATH"

echo "==> Building $BIN_NAME..."
go build -o "$BIN_NAME" ./cmd/llm-bridge-server
echo "    built: $(ls -lh "$BIN_NAME" | awk '{print $5}')"

echo "==> Stopping $SERVICE..."
sudo systemctl stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binary to $SYSTEM_BIN..."
sudo cp "$BIN_NAME" "$SYSTEM_BIN"

echo "==> Installing service file..."
sudo cp "$REPO_DIR/llm-bridge.service" /etc/systemd/system/"$SERVICE"
sudo systemctl daemon-reload

echo "==> Starting $SERVICE..."
sudo systemctl start "$SERVICE"

echo "==> Verifying..."
sleep 2
if systemctl is-active --quiet "$SERVICE"; then
  echo "    $SERVICE is running"
  journalctl -u "$SERVICE" -n 5 --no-pager 2>&1 | grep -v '^--'
else
  echo "ERROR: $SERVICE failed to start"
  journalctl -u "$SERVICE" -n 15 --no-pager 2>&1
  exit 1
fi

echo "==> Done."
