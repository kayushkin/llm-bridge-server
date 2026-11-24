#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="grant-store.service"
BINARY="grant-store"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test ./...

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/grant-store
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"

# A set GRANT_STORE_ variable that settings.go does not declare, or a missing
# service token, stops the new binary at boot. Ask before the old one is
# stopped: build the registry from the running service's own environment. The
# test prints a verdict, never a value.
echo "==> Checking the running service's environment against the declared settings..."
live_pid="$(systemctl --user show -p MainPID --value "$SERVICE")"
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

# A running process is not a working one: prove the API answers and that the
# vocabulary routes carry what the UI builds its pickers from.
echo "==> Smoke-checking the API..."
ADDR="${GRANT_STORE_ADDR:-127.0.0.1:8315}"
BASE="http://${ADDR}"
curl -sfS "$BASE/health" >/dev/null || { echo "ERROR: /health did not answer"; exit 1; }
curl -sfS "$BASE/relations" | grep -q '"can_use"' || { echo "ERROR: /relations did not list can_use"; exit 1; }
# /grants is gated, so the smoke check presents the service token the unit
# carries — reading it the same way the unit does, from the host-local file, so
# the check exercises the real credential rather than a copy of it.
TOKEN_FILE="${GRANT_STORE_TOKEN_FILE:-$HOME/.config/principal-gating-tokens.env}"
SERVICE_TOKEN="${GRANT_STORE_SERVICE_TOKEN:-}"
if [ -z "$SERVICE_TOKEN" ] && [ -r "$TOKEN_FILE" ]; then
  SERVICE_TOKEN="$(sed -n 's/^GRANT_STORE_SERVICE_TOKEN=//p' "$TOKEN_FILE" | head -1)"
fi
if [ -z "$SERVICE_TOKEN" ]; then
  echo "ERROR: no GRANT_STORE_SERVICE_TOKEN to smoke-check with (looked in the environment and $TOKEN_FILE)"
  exit 1
fi
curl -sfS -H "X-Grant-Store-Service-Token: $SERVICE_TOKEN" "$BASE/grants" >/dev/null || { echo "ERROR: /grants did not answer the service token"; exit 1; }
curl -s -o /dev/null -w '%{http_code}' "$BASE/grants" | grep -q '^401$' || { echo "ERROR: /grants answered an unauthenticated call with something other than 401"; exit 1; }
curl -sfS -H "X-Grant-Store-Service-Token: $SERVICE_TOKEN" "$BASE/settings" | grep -q '"service":"grant-store"' || { echo "ERROR: /settings did not describe the service to the service token"; exit 1; }
curl -s -o /dev/null -w '%{http_code}' "$BASE/settings" | grep -q '^401$' || { echo "ERROR: /settings answered an unauthenticated call with something other than 401"; exit 1; }
echo "    /health and /relations answered; /grants and /settings answered the service token and 401'd without it"

# The bind is part of the contract: this service has no auth and must not be
# reachable off-host. Fail the deploy if it is listening anywhere else.
echo "==> Checking the bind..."
if ss -tlnH "sport = :${ADDR##*:}" | grep -q '127\.0\.0\.1:'; then
  echo "    bound to 127.0.0.1"
else
  echo "ERROR: grant-store is not bound to 127.0.0.1:"
  ss -tlnp "sport = :${ADDR##*:}"
  exit 1
fi

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
