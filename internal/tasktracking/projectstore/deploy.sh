#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed. It lives in
# healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="project-store.service"
BINARY="project-store"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"
DROP_IN="$HOME/.config/systemd/user/$SERVICE.d"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test ./...

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/project-store

# A binary with no vcs.revision cannot be traced to a commit: go build writes
# none when .git is not a directory (a worktree), and does not fail.
echo "==> Checking provenance..."
vcs_revision="$(go version -m "$BINARY" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
if [ -z "$vcs_revision" ]; then
  echo "    REFUSING TO INSTALL: no vcs.revision in $BINARY. Build from the main clone." >&2
  exit 1
fi
echo "    vcs.revision=$vcs_revision"

# A set PROJECT_STORE_ variable that settings.go does not declare stops the
# new binary at boot. Ask before the old one is stopped.
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

echo "==> Installing binaries to $BIN_DIR..."
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"

# The service refuses to start without kanban-store's service token, which
# only a host-local drop-in may hold.
if ! grep -qs 'KANBAN_STORE_SERVICE_TOKEN\|EnvironmentFile' "$DROP_IN"/*.conf; then
  echo "ERROR: no drop-in in $DROP_IN gives the service KANBAN_STORE_SERVICE_TOKEN" >&2
  exit 1
fi

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
ADDR="$(sed -n 's/^Environment=PROJECT_STORE_ADDR=//p' "$UNIT_SRC")"
[ -n "$ADDR" ] || { echo "ERROR: $UNIT_SRC sets no PROJECT_STORE_ADDR"; exit 1; }
curl -sfS "http://$ADDR/health" >/dev/null || { echo "ERROR: /health did not answer"; exit 1; }
curl -sfS "http://$ADDR/vocabulary" >/dev/null || { echo "ERROR: /vocabulary did not answer"; exit 1; }
curl -sfS "http://$ADDR/digest" >/dev/null || { echo "ERROR: /digest did not answer: can it reach kanban-store, work-graph-store and repo-store?"; exit 1; }
echo "    $SERVICE is running and answering"

echo "==> Done."

# Last act: write this deploy to repo-store's ledger.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
