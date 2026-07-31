#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BUS_DIR="$REPO_DIR/../bus"
BIN_DIR="$HOME/bin"
BUILD_DIR="$REPO_DIR/build"
SERVICE="scheduler.service"

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

# Every command in cmd/ is deployed. The list used to be written out by hand
# here and again in .gitignore, and both copies drifted: cmd/reminder-coordinator
# was in neither, so scheduler job 32 ran a binary this script had never built
# and only a hand build ever updated it. Read the directory instead.
commands=()
for cmd_dir in "$REPO_DIR"/cmd/*/; do
  # An unmatched glob expands to itself, so check before trusting it.
  [ -d "$cmd_dir" ] || continue
  commands+=("$(basename "$cmd_dir")")
done

if [ ${#commands[@]} -eq 0 ]; then
  echo "ERROR: no commands found under $REPO_DIR/cmd"
  exit 1
fi

echo "==> Building ${#commands[@]} commands: ${commands[*]}"
mkdir -p "$BUILD_DIR"
for command_name in "${commands[@]}"; do
  echo "==> Building $command_name..."
  go build -o "$BUILD_DIR/$command_name" "./cmd/$command_name"
  echo "    built: $(ls -lh "$BUILD_DIR/$command_name" | awk '{print $5}')"
done

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binaries to $BIN_DIR..."
for command_name in "${commands[@]}"; do
  cp "$BUILD_DIR/$command_name" "$BIN_DIR/$command_name"
done

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
