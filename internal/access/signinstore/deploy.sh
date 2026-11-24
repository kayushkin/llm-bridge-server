#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="sign-in-store.service"
BINARY="sign-in-store"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"
SECRETS_FILE="$HOME/.config/sign-in-store/sign-in-store.env"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test ./...

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/sign-in-store
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"

# The secrets live in one host-local file. The first deploy writes it: a new
# service token, and the auth-store token from the deploying environment. A
# later deploy leaves it alone, so the token dash holds keeps working.
echo "==> Checking the secrets file..."
if [ ! -f "$SECRETS_FILE" ]; then
  if [ -z "${AUTH_STORE_TOKEN:-}" ]; then
    echo "ERROR: $SECRETS_FILE does not exist and AUTH_STORE_TOKEN is not set to write it with"
    exit 1
  fi
  mkdir -p "$(dirname "$SECRETS_FILE")"
  chmod 700 "$(dirname "$SECRETS_FILE")"
  umask 077
  {
    echo "# sign-in-store secrets. Host-local, never committed."
    echo "SIGN_IN_STORE_SERVICE_TOKEN=$(openssl rand -hex 32)"
    echo "AUTH_STORE_TOKEN=$AUTH_STORE_TOKEN"
  } > "$SECRETS_FILE"
  echo "    wrote $SECRETS_FILE (mode 600)"
else
  echo "    $SECRETS_FILE exists"
fi

# A set SIGN_IN_STORE_ variable that settings.go does not declare stops the new
# binary at boot. Ask before the old one is stopped. The test prints a verdict,
# never a value.
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

echo "==> Installing the binary to $BIN_DIR..."
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

# A running process is not a working one: prove the routes answer, and that
# the guarded ones refuse a caller with no credential.
echo "==> Smoke-checking the API..."
ADDR="$(sed -n 's/^Environment=SIGN_IN_STORE_ADDR=//p' "$UNIT_SRC")"
if [ -z "$ADDR" ]; then
  echo "ERROR: $UNIT_SRC sets no SIGN_IN_STORE_ADDR"
  exit 1
fi
BASE="http://${ADDR}"
curl -sfS "$BASE/health" >/dev/null || { echo "ERROR: /health did not answer"; exit 1; }
curl -sfS "$BASE/settings" >/dev/null || { echo "ERROR: /settings did not answer"; exit 1; }
status="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/session")"
[ "$status" = "401" ] || { echo "ERROR: GET /session with no cookie answered $status, not 401"; exit 1; }
status="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/sessions")"
[ "$status" = "401" ] || { echo "ERROR: GET /sessions with no token answered $status, not 401"; exit 1; }
echo "    /health and /settings answered; /session and /sessions refused a caller with nothing"

echo "==> Checking the bind..."
if ss -tlnH "sport = :${ADDR##*:}" | grep -q '127\.0\.0\.1:'; then
  echo "    bound to 127.0.0.1"
else
  echo "ERROR: sign-in-store is not bound to 127.0.0.1:"
  ss -tlnp "sport = :${ADDR##*:}"
  exit 1
fi

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
