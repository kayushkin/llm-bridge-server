#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="file-store.service"
BINARY="file-store"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"
TOKEN_FILE="$HOME/.config/file-store-tokens.env"
DROP_IN="$HOME/.config/systemd/user/$SERVICE.d/service-token.conf"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

# The token is host-local and never tracked. Refuse to deploy a service that
# would start without one rather than let the unit crash-loop.
if [ ! -r "$TOKEN_FILE" ] || [ ! -r "$DROP_IN" ]; then
  echo "ERROR: $TOKEN_FILE (mode 600, FILE_STORE_SERVICE_TOKEN=…) and $DROP_IN (EnvironmentFile=$TOKEN_FILE) must exist. See README, Running it."
  exit 1
fi
if [ "$(stat -c '%a' "$TOKEN_FILE")" != "600" ]; then
  echo "ERROR: $TOKEN_FILE is mode $(stat -c '%a' "$TOKEN_FILE"); it holds a token that reads every file and must be 600"
  exit 1
fi

echo "==> Testing..."
go test ./...

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/file-store
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"

echo "==> Installing systemd unit..."
mkdir -p "$(dirname "$UNIT_DEST")"
cp "$UNIT_SRC" "$UNIT_DEST"

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binary to $BIN_DIR..."
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"

echo "==> Starting $SERVICE..."
systemctl --user daemon-reload
systemctl --user enable "$SERVICE" >/dev/null
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

# A running process is not a working one: prove a file goes in and comes back.
echo "==> Smoke-checking the API..."
ADDR="$(sed -n 's/^Environment=FILE_STORE_ADDR=//p' "$UNIT_SRC" | head -1)"
BASE="http://${ADDR}"
SERVICE_TOKEN="$(sed -n 's/^FILE_STORE_SERVICE_TOKEN=//p' "$TOKEN_FILE" | head -1)"
curl -sfS "$BASE/health" >/dev/null || { echo "ERROR: /health did not answer"; exit 1; }
curl -s -o /dev/null -w '%{http_code}' "$BASE/files" | grep -q '^401$' || { echo "ERROR: /files answered an unauthenticated call with something other than 401"; exit 1; }
PROBE="deploy smoke check $(date -u +%FT%TZ)"
FILE_ID="$(printf '%s' "$PROBE" | curl -sfS -X POST --data-binary @- -H 'Content-Type: text/plain' -H "X-File-Store-Service-Token: $SERVICE_TOKEN" \
  "$BASE/files?owner_service=file-store-deploy&owner_ref=smoke-check&filename=smoke-check.txt" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
BACK="$(curl -sfS -H "X-File-Store-Service-Token: $SERVICE_TOKEN" "$BASE/files/$FILE_ID/content")"
[ "$BACK" = "$PROBE" ] || { echo "ERROR: $FILE_ID came back as '$BACK', not what was uploaded"; exit 1; }
curl -sfS -o /dev/null -X DELETE -H "X-File-Store-Service-Token: $SERVICE_TOKEN" "$BASE/files/$FILE_ID?hard=true" || { echo "ERROR: could not purge the probe $FILE_ID"; exit 1; }
echo "    /health answered; /files 401'd without the token; $FILE_ID went in, came back byte for byte, and was purged"

# The bind is part of the contract: whoever reaches this service with the token
# reads every file, so it must not listen anywhere but loopback.
echo "==> Checking the bind..."
if ss -tlnH "sport = :${ADDR##*:}" | grep -q '127\.0\.0\.1:'; then
  echo "    bound to 127.0.0.1"
else
  echo "ERROR: file-store is not bound to 127.0.0.1:"
  ss -tlnp "sport = :${ADDR##*:}"
  exit 1
fi

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
