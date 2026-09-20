#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )
eval "$("$HOME/.local/bin/mise" activate bash)"

# Required for systemctl --user to reach the session bus from non-login shells.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

BIN="$HOME/bin/mailstack"

cd "$(dirname "$0")"

if ! git diff-index --quiet HEAD -- 2>/dev/null; then
    echo "==> Refusing to deploy: uncommitted changes in $(pwd)"
    git status --short
    exit 1
fi

echo "==> Compiling mailstack..."
go build -o /tmp/mailstack-build ./cmd/mailstack/

# A set variable under one of the names settings.go owns that it does not
# declare stops the new binary at boot. Ask before the old one is stopped: build
# the registry from the running service's own environment. The test prints a
# verdict, never a value.
echo "==> Checking the running service's environment against the declared settings..."
live_pid="$(systemctl --user show -p MainPID --value mailstack.service)"
if [ -n "$live_pid" ] && [ "$live_pid" != "0" ]; then
  go test -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' . -args -live-environment-file="/proc/$live_pid/environ"
else
  echo "    mailstack.service is not running, so there is no environment to check"
fi

echo "==> Pushing..."
git push

echo "==> Deploying..."
systemctl --user stop mailstack.service 2>/dev/null || true
# Atomic install: cp then mv avoids ETXTBSY when the old binary is still
# mmapped during the systemd stop grace window.
cp /tmp/mailstack-build "$BIN.new"
mv -f "$BIN.new" "$BIN"
systemctl --user start mailstack.service

echo "==> Verifying..."
sleep 2
systemctl --user is-active mailstack.service

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
