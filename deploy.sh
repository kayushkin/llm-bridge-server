#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BUS_DIR="$REPO_DIR/../bus"
BIN_DIR="$HOME/bin"
SERVICE="scheduler.service"
BINARY="scheduler"

cd "$REPO_DIR"

# Ensure bus dependency is available (local replace directive)
if [ ! -d "$BUS_DIR" ]; then
  echo "ERROR: bus repo not found at $BUS_DIR"
  exit 1
fi

# Add go to PATH if managed by mise
export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/scheduler
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"

echo "==> Building autoworker..."
go build -o autoworker ./cmd/autoworker
echo "    built: $(ls -lh autoworker | awk '{print $5}')"

echo "==> Building kanban-curator..."
go build -o kanban-curator ./cmd/kanban-curator
echo "    built: $(ls -lh kanban-curator | awk '{print $5}')"

echo "==> Building kanban-classifier..."
go build -o kanban-classifier ./cmd/kanban-classifier
echo "    built: $(ls -lh kanban-classifier | awk '{print $5}')"

echo "==> Building kanban-scoper..."
go build -o kanban-scoper ./cmd/kanban-scoper
echo "    built: $(ls -lh kanban-scoper | awk '{print $5}')"

echo "==> Building kanban-dispatcher..."
go build -o kanban-dispatcher ./cmd/kanban-dispatcher
echo "    built: $(ls -lh kanban-dispatcher | awk '{print $5}')"

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binaries to $BIN_DIR..."
cp "$BINARY" "$BIN_DIR/$BINARY"
cp autoworker "$BIN_DIR/autoworker"
cp kanban-curator "$BIN_DIR/kanban-curator"
cp kanban-classifier "$BIN_DIR/kanban-classifier"
cp kanban-scoper "$BIN_DIR/kanban-scoper"
cp kanban-dispatcher "$BIN_DIR/kanban-dispatcher"

echo "==> Starting $SERVICE..."
systemctl --user daemon-reload
systemctl --user start "$SERVICE"

echo "==> Verifying..."
sleep 2
if systemctl --user is-active --quiet "$SERVICE"; then
  echo "    $SERVICE is running"
  journalctl --user -u "$SERVICE" -n 5 --no-pager 2>&1 | grep -v '^--'
else
  echo "ERROR: $SERVICE failed to start"
  journalctl --user -u "$SERVICE" -n 15 --no-pager 2>&1
  exit 1
fi

echo "==> Done."
