#!/usr/bin/env bash
set -euo pipefail

# Re-exec inside a fresh systemd transient unit so the deploy survives
# `systemctl stop llm-bridge.service`. When the deploy is triggered by an
# agent running inside llm-bridge.service, the agent's bash is in the
# service's cgroup; `setsid nohup` does NOT escape systemd's control-group
# kill, so the stop takes the deploy with it. A transient unit lives in
# its own cgroup under system.slice and is untouched by the service stop.
if [ -z "${DEPLOY_DETACHED:-}" ]; then
  # Log lives under $HOME (not /tmp) because systemd transient units get a
  # PrivateTmp namespace, so the unit can't write to the host's /tmp.
  LOG="$HOME/.cache/llm-bridge-deploy.log"
  mkdir -p "$(dirname "$LOG")"
  : >"$LOG"
  UNIT="llm-bridge-deploy-$$.service"
  # Resolve $0 to an absolute path — the transient unit doesn't inherit our
  # working directory, so a relative ./deploy.sh would fail to find itself.
  SCRIPT="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  sudo systemd-run \
    --collect \
    --unit="$UNIT" \
    --description="llm-bridge deploy ($USER)" \
    --uid="$(id -u)" \
    --gid="$(id -g)" \
    --setenv=DEPLOY_DETACHED=1 \
    --setenv=HOME="$HOME" \
    --setenv=PATH="$PATH" \
    --property=StandardOutput=append:"$LOG" \
    --property=StandardError=append:"$LOG" \
    bash "$SCRIPT" "$@" >/dev/null
  echo "detached deploy (unit=$UNIT), tail -f $LOG"
  echo "  status: systemctl status $UNIT"
  echo "  logs:   journalctl -u $UNIT -f"
  exit 0
fi

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_NAME="llm-bridge"
SYSTEM_BIN="/usr/local/bin/$BIN_NAME"
SERVICE="llm-bridge.service"

cd "$REPO_DIR"

# Add go to PATH if managed by mise
export PATH="$HOME/.local/share/mise/shims:$PATH"

echo "==> Building $BIN_NAME..."
go build -o "$BIN_NAME" ./cmd/llm-bridge-server
echo "    built: $(ls -lh "$BIN_NAME" | awk '{print $5}')"

echo "==> Stopping $SERVICE..."
sudo systemctl stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binary to $SYSTEM_BIN..."
sudo cp "$BIN_NAME" "$SYSTEM_BIN"

echo "==> Installing service file..."
# The committed unit file uses __USER__ / __HOME__ placeholders so the repo
# stays portable. Expand them for the local machine before installing.
TMP_SVC=$(mktemp)
sed -e "s|__USER__|$USER|g" -e "s|__HOME__|$HOME|g" \
    "$REPO_DIR/llm-bridge.service" > "$TMP_SVC"
sudo cp "$TMP_SVC" /etc/systemd/system/"$SERVICE"
rm -f "$TMP_SVC"
sudo systemctl daemon-reload

echo "==> Starting $SERVICE..."
sudo systemctl start "$SERVICE"

echo "==> Verifying..."
sleep 2
if systemctl is-active --quiet "$SERVICE"; then
  echo "    $SERVICE is running"
  journalctl -u "$SERVICE" -n 5 --no-pager 2>&1 | grep -v '^--'
else
  echo "ERROR: $SERVICE failed to start"
  journalctl -u "$SERVICE" -n 15 --no-pager 2>&1
  exit 1
fi

echo "==> Done."
