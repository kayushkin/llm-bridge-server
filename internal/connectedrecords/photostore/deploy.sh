#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone,
# default branch, clean, pushed, not behind). It lives in
# healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="photo-store.service"
BINARY="photo-store"
TOOLS=(photo-import photo-import-messages photo-import-facebook photo-match-people photo-group-faces photo-guess-face-groups)
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"
cd "$REPO_DIR"
export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test ./...

echo "==> Building $BINARY and ${TOOLS[*]}..."
go build -o "$BINARY" ./cmd/photo-store
for tool in "${TOOLS[@]}"; do
  go build -o "$tool" "./cmd/$tool"
done

echo "==> Checking provenance..."
vcs_revision="$(go version -m "$BINARY" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
if [ -z "$vcs_revision" ]; then
  echo "    REFUSING TO INSTALL: no vcs.revision in $BINARY. Build from the main clone." >&2
  exit 1
fi
echo "    vcs.revision=$vcs_revision"

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

echo "==> Installing binaries to $BIN_DIR..."
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"
for tool in "${TOOLS[@]}"; do
  install -m 755 "$tool" "$BIN_DIR/$tool"
  echo "    installed: $BIN_DIR/$tool"
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
ADDR="$(sed -n 's/^Environment=PHOTO_STORE_ADDR=//p' "$UNIT_SRC")"
[ -n "$ADDR" ] || { echo "ERROR: $UNIT_SRC sets no PHOTO_STORE_ADDR"; exit 1; }
health="$(curl -sfS "http://$ADDR/health")" || { echo "ERROR: /health did not answer"; exit 1; }
printf '%s' "$health" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["status"]=="ok" else 1)' || { echo "ERROR: /health is not ok: $health"; exit 1; }
echo "    /health is ok"

echo "==> Checking the bind..."
if ss -tlnH "sport = :${ADDR##*:}" | awk '{print $4}' | grep -qv '^127\.0\.0\.1:'; then
  echo "ERROR: $SERVICE listens beyond 127.0.0.1; it has no auth" >&2
  exit 1
fi
echo "    bound to 127.0.0.1"

echo "==> Done."
( cd "$REPO_DIR" && "$HOME/bin/deploy-gate" record )
