#!/usr/bin/env bash
set -euo pipefail
eval "$("$HOME/.local/bin/mise" activate bash)"

# Required for systemctl --user to reach the session bus from non-login shells.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

BIN="$HOME/bin/mailstack"

cd "$(dirname "$0")"

if ! git diff-index --quiet HEAD -- 2>/dev/null; then
    echo "==> Refusing to deploy: uncommitted changes in $(pwd)"
    git status --short
    exit 1
fi

echo "==> Compiling mailstack..."
go build -o /tmp/mailstack-build ./cmd/mailstack/

echo "==> Pushing..."
git push

echo "==> Deploying..."
systemctl --user stop mailstack.service 2>/dev/null || true
# Atomic install: cp then mv avoids ETXTBSY when the old binary is still
# mmapped during the systemd stop grace window.
cp /tmp/mailstack-build "$BIN.new"
mv -f "$BIN.new" "$BIN"
systemctl --user start mailstack.service

echo "==> Verifying..."
sleep 2
systemctl --user is-active mailstack.service

echo "==> Done."
