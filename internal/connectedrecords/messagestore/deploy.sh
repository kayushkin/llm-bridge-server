#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone,
# default branch, clean, pushed, not behind). It lives in
# healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="message-store.service"
BINARY="message-store"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"
cd "$REPO_DIR"
# Every command under cmd/ other than the server is a tool installed beside it.
TOOLS=()
for tool_dir in cmd/*/; do
  tool="$(basename "$tool_dir")"
  [ "$tool" = "$BINARY" ] || TOOLS+=("$tool")
done
export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test -tags sqlite_fts5 ./...

echo "==> Building $BINARY and ${TOOLS[*]}..."
go build -tags sqlite_fts5 -o "$BINARY" ./cmd/message-store
for tool in "${TOOLS[@]}"; do
  go build -tags sqlite_fts5 -o "$tool" "./cmd/$tool"
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
  go test -tags sqlite_fts5 -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' . -args -live-environment-file="/proc/$live_pid/environ"
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
ADDR="$(sed -n 's/^Environment=MESSAGE_STORE_ADDR=//p' "$UNIT_SRC")"
[ -n "$ADDR" ] || { echo "ERROR: $UNIT_SRC sets no MESSAGE_STORE_ADDR"; exit 1; }
# Opening the database runs the start-up migrations, which take a few
# seconds on a large one; wait for /health while the unit stays up.
health=""
for attempt in $(seq 1 60); do
  if health="$(curl -sf "http://$ADDR/health")"; then break; fi
  if ! systemctl --user is-active --quiet "$SERVICE"; then
    echo "ERROR: $SERVICE stopped before /health answered"
    journalctl --user -u "$SERVICE" -n 20 --no-pager 2>&1
    exit 1
  fi
  sleep 1
done
[ -n "$health" ] || { echo "ERROR: /health did not answer within 60 s"; exit 1; }
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
