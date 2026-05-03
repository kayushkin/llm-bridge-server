#!/usr/bin/env bash
set -euo pipefail
eval "$("$HOME/.local/bin/mise" activate bash)"

# Required for systemctl --user to reach the session bus when invoked from
# non-login shells (e.g. agent/CI invocations).
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$HOME/bin/permission-store"
UNIT_NAME="permission-store.service"
UNIT_DIR="$HOME/.config/systemd/user"

cd "$REPO_DIR"

echo "==> Compiling permission-store..."
go build -o /tmp/permission-store-build ./cmd/permission-store/

echo "==> Pushing..."
git push 2>/dev/null || echo "    (no remote or already up-to-date)"

echo "==> Stopping service..."
systemctl --user stop "$UNIT_NAME" 2>/dev/null || true
sleep 1

echo "==> Installing binary to $BIN..."
mkdir -p "$(dirname "$BIN")"
cp /tmp/permission-store-build "$BIN.new"
mv -f "$BIN.new" "$BIN"

echo "==> Installing systemd unit..."
mkdir -p "$UNIT_DIR"
cp "$REPO_DIR/$UNIT_NAME" "$UNIT_DIR/$UNIT_NAME"
systemctl --user daemon-reload
systemctl --user enable "$UNIT_NAME" 2>/dev/null || true

echo "==> Starting service..."
systemctl --user start "$UNIT_NAME"

echo "==> Verifying..."
sleep 2
systemctl --user is-active "$UNIT_NAME"

echo "==> Health check..."
curl -fsS http://127.0.0.1:8304/healthz > /dev/null || {
  echo "FAILED — health check did not respond"
  exit 1
}

echo "==> Done."
