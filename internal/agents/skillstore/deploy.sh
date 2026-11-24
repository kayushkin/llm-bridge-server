#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="skill-store.service"
BINARY="skill-store"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/skill-store
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"

# A set SKILL_STORE_ADDR…, SKILL_STORE_DATA_DIR… or SKILL_STORE_INSTALL_DIR…
# variable that settings.go does not declare stops the new binary at boot. Ask
# before the old one is stopped: build the registry from the running service's
# own environment. The test prints a verdict, never a value.
echo "==> Checking the running service's environment against the declared settings..."
live_pid="$(systemctl --user show -p MainPID --value "$SERVICE")"
if [ -n "$live_pid" ] && [ "$live_pid" != "0" ]; then
  go test -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' . -args -live-environment-file="/proc/$live_pid/environ"
else
  echo "    $SERVICE is not running, so there is no environment to check"
fi

# Checked BEFORE the install, for the same reason the other pre-install checks are:
# an unidentifiable binary compiles perfectly and reads clean in the log, so
# installing first would put it in front of live callers and only then tell us it
# cannot be traced back to a commit.
echo "==> Checking provenance..."
buildinfo="$(go version -m "$BINARY")"
vcs_revision="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
vcs_modified="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.modified$/ {print $2}')"
if [ -z "$vcs_revision" ]; then
    echo "    REFUSING TO INSTALL: this binary carries no vcs.revision, so nothing can tie" >&2
    echo "    it back to a commit. 'go build' writes no VCS stamp when it cannot find a .git" >&2
    echo "    DIRECTORY, and it does not fail when that happens -- not even with -buildvcs=true." >&2
    echo "    The usual cause is building from a git worktree, whose .git is a pointer file." >&2
    echo "    Build from a real clone or checkout instead." >&2
    exit 1
fi
echo "    vcs.revision=$vcs_revision"
if [ "$vcs_modified" = "true" ]; then
    echo "    WARNING: built from a DIRTY tree (vcs.modified=true). $vcs_revision names the" >&2
    echo "    commit this binary was built NEAR, not the source it was built FROM, and that" >&2
    echo "    source is not recoverable from any commit. Commit first for a reproducible build." >&2
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
# `is-active` only proves the process exists. Poll the HTTP surface: a binary
# that boots and then fails to answer is the failure this check has to catch.
PORT="$(systemctl --user show -p Environment --value "$SERVICE" \
  | tr ' ' '\n' | sed -n 's/^SKILL_STORE_ADDR=.*://p')"
PORT="${PORT:-8301}"
READY=""
for _ in $(seq 1 30); do
  if ! systemctl --user is-active --quiet "$SERVICE"; then
    echo "ERROR: $SERVICE failed to start"
    journalctl --user -u "$SERVICE" -n 20 --no-pager 2>&1
    exit 1
  fi
  if [ "$(curl -fsS --max-time 2 "http://127.0.0.1:$PORT/health" 2>/dev/null)" = '{"status":"ok"}' ]; then
    READY=1
    break
  fi
  sleep 0.5
done
if [ -z "$READY" ]; then
  echo "ERROR: $SERVICE is running but did not answer /health on :$PORT within 15s"
  journalctl --user -u "$SERVICE" -n 20 --no-pager 2>&1
  exit 1
fi
# The registry must actually serve its rows, not just a static /health.
SKILL_COUNT="$(curl -fsS --max-time 5 "http://127.0.0.1:$PORT/skills" | jq -r 'length')"
echo "    $SERVICE answers on :$PORT ($SKILL_COUNT skills registered)"
journalctl --user -u "$SERVICE" -n 5 --no-pager 2>&1 | grep -v '^--' || true

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
