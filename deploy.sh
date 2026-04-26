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

echo "==> Installing host-local drop-in..."
# The committed unit intentionally has no Environment=PATH/HOME so it stays
# portable. Inject the deploying user's live PATH and HOME via a drop-in so
# the service can exec tools managed by the local toolchain (e.g. mise shims
# for `claude`, `node`). Without this systemd uses its default PATH and
# harness spawning fails with "executable file not found".
DROPIN_DIR="/etc/systemd/system/$SERVICE.d"
sudo mkdir -p "$DROPIN_DIR"
DEPLOY_PATH=$(echo "$PATH" | tr ':' '\n' | awk '!seen[$0]++' | paste -sd:)
TMP_DROPIN=$(mktemp)
cat > "$TMP_DROPIN" <<EOF
[Service]
Environment=PATH=$DEPLOY_PATH
Environment=HOME=$HOME
EOF
sudo cp "$TMP_DROPIN" "$DROPIN_DIR/local.conf"
rm -f "$TMP_DROPIN"
# Remove the hand-rolled drop-in left over from before deploy.sh owned this.
sudo rm -f "$DROPIN_DIR/path.conf"

sudo systemctl daemon-reload

echo "==> Starting $SERVICE..."
sudo systemctl start "$SERVICE"

echo "==> Verifying..."
sleep 2
if ! systemctl is-active --quiet "$SERVICE"; then
  echo "ERROR: $SERVICE failed to start"
  journalctl -u "$SERVICE" -n 15 --no-pager 2>&1
  exit 1
fi
echo "    $SERVICE is running"
journalctl -u "$SERVICE" -n 5 --no-pager 2>&1 | grep -v '^--'

echo "==> Smoke test..."
# 1. HTTP up — the listener is bound and serving.
if ! curl -fsS http://localhost:8160/sessions >/dev/null 2>&1; then
  echo "ERROR: $SERVICE not responding on :8160/sessions"
  journalctl -u "$SERVICE" -n 30 --no-pager
  exit 1
fi
# 2. Every dir from the drop-in's PATH is present in the running service's
# PATH. This is the exact regression that took out harness spawning when
# the unit was first templatized — without mise shims reachable, the
# service couldn't exec `claude`.
SVC_PID=$(systemctl show -p MainPID --value "$SERVICE")
if [ -n "$SVC_PID" ] && [ "$SVC_PID" != "0" ]; then
  SVC_PATH=$(tr '\0' '\n' < /proc/"$SVC_PID"/environ 2>/dev/null \
             | grep '^PATH=' | head -1 | cut -d= -f2-)
  MISSING=""
  IFS=':' read -ra DEP_DIRS <<< "$DEPLOY_PATH"
  for dir in "${DEP_DIRS[@]}"; do
    case ":$SVC_PATH:" in
      *":$dir:"*) ;;
      *) MISSING="$MISSING $dir" ;;
    esac
  done
  if [ -n "$MISSING" ]; then
    echo "ERROR: service PATH missing dirs from drop-in:$MISSING"
    echo "  service PATH: $SVC_PATH"
    echo "  drop-in PATH: $DEPLOY_PATH"
    exit 1
  fi
fi
echo "    smoke test OK"

echo "==> Done."
