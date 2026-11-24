#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone,
# default branch, clean, pushed, not behind). It lives in
# healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="reference-store.service"
BINARY="reference-store"
TOOLS=()
GO_TAGS=""
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"
cd "$REPO_DIR"
export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test -tags "$GO_TAGS" ./...

echo "==> Building $BINARY..."
go build -tags "$GO_TAGS" -o "$BINARY" ./cmd/reference-store
for tool in "${TOOLS[@]+"${TOOLS[@]}"}"; do
  go build -tags "$GO_TAGS" -o "$tool" "./cmd/$tool"
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
  go test -tags "$GO_TAGS" -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' . -args -live-environment-file="/proc/$live_pid/environ"
else
  echo "    $SERVICE is not running, so there is no environment to check"
fi

echo "==> Installing systemd unit..."
mkdir -p "$(dirname "$UNIT_DEST")"
cp "$UNIT_SRC" "$UNIT_DEST"

echo "==> Writing the token drop-in..."
# Each token is copied from the file that owns it, under this service's own
# variable names, so no file's other secrets reach the process. Values are
# never printed. The service token lives beside the other gated services'
# tokens, where llm-bridge-server and grant-store read it; it is made once.
GATED_TOKENS="$HOME/.config/principal-gating-tokens.env"
AUTH_STORE_UNIT="$HOME/.config/systemd/user/auth-store.service"
DROP_IN_DIR="$UNIT_DEST.d"
[ "$(stat -c '%a' "$GATED_TOKENS")" = "600" ] || { echo "$GATED_TOKENS is not mode 600; refusing to read it" >&2; exit 1; }
if ! grep -q '^REFERENCE_STORE_SERVICE_TOKEN=' "$GATED_TOKENS"; then
  printf 'REFERENCE_STORE_SERVICE_TOKEN=%s\n' "$(openssl rand -hex 32)" >> "$GATED_TOKENS"
  echo "    made REFERENCE_STORE_SERVICE_TOKEN in $GATED_TOKENS"
fi
service_token="$(sed -n 's/^REFERENCE_STORE_SERVICE_TOKEN=//p' "$GATED_TOKENS")"
bridge_token="$(sed -n 's/^LLMBRIDGE_SERVICE_TOKEN=//p' "$GATED_TOKENS")"
auth_store_token="$(sed -n 's/^Environment=AUTHSTORE_TOKEN=//p' "$AUTH_STORE_UNIT")"
[ -n "$service_token" ] || { echo "no REFERENCE_STORE_SERVICE_TOKEN in $GATED_TOKENS" >&2; exit 1; }
[ -n "$bridge_token" ] || { echo "no LLMBRIDGE_SERVICE_TOKEN in $GATED_TOKENS" >&2; exit 1; }
[ -n "$auth_store_token" ] || { echo "no AUTHSTORE_TOKEN in $AUTH_STORE_UNIT" >&2; exit 1; }
mkdir -p "$DROP_IN_DIR"
umask 077
printf '[Service]\nEnvironment=REFERENCE_STORE_SERVICE_TOKEN=%s\nEnvironment=REFERENCE_STORE_BRIDGE_SERVICE_TOKEN=%s\nEnvironment=REFERENCE_STORE_AUTH_STORE_TOKEN=%s\n' \
  "$service_token" "$bridge_token" "$auth_store_token" > "$DROP_IN_DIR/tokens.conf"
chmod 600 "$DROP_IN_DIR/tokens.conf"
umask 022

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binaries to $BIN_DIR..."
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"
for tool in "${TOOLS[@]+"${TOOLS[@]}"}"; do
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
ADDR="$(sed -n 's/^Environment=REFERENCE_STORE_ADDR=//p' "$UNIT_SRC")"
[ -n "$ADDR" ] || { echo "ERROR: $UNIT_SRC sets no REFERENCE_STORE_ADDR"; exit 1; }
health="$(curl -sfS "http://$ADDR/health")" || { echo "ERROR: /health did not answer"; exit 1; }
printf '%s' "$health" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["status"]=="ok" else 1)' || { echo "ERROR: /health is not ok: $health"; exit 1; }
echo "    /health is ok"

echo "==> Checking the bind..."
if ss -tlnH "sport = :${ADDR##*:}" | awk '{print $4}' | grep -qv '^127\.0\.0\.1:'; then
  echo "ERROR: $SERVICE listens beyond 127.0.0.1; it trusts X-Principal-Id" >&2
  exit 1
fi
echo "    bound to 127.0.0.1"

echo "==> Done."
( cd "$REPO_DIR" && "$HOME/bin/deploy-gate" record )
