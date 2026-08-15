#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BUS_DIR="$REPO_DIR/../bus"
BIN_DIR="$HOME/bin"
BUILD_DIR="$REPO_DIR/build"
SERVICE="scheduler.service"

cd "$REPO_DIR"

# Ensure bus dependency is available (local replace directive)
if [ ! -d "$BUS_DIR" ]; then
  echo "ERROR: bus repo not found at $BUS_DIR"
  exit 1
fi

# Add go to PATH if managed by mise
export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

# Provenance, checked per command BEFORE any of them is installed. An unidentifiable
# binary compiles perfectly and reads clean in the log, so installing first would put
# it in front of the live cron fleet and only then tell us it cannot be traced to a
# commit. This script builds every command under cmd/, so the check is a function
# called once per target rather than a single block.
check_provenance() {
  local binary="$1"
  local buildinfo vcs_revision vcs_modified
  buildinfo="$(go version -m "$binary")"
  vcs_revision="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
  vcs_modified="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.modified$/ {print $2}')"
  if [ -z "$vcs_revision" ]; then
    echo "    REFUSING TO INSTALL $binary: it carries no vcs.revision, so nothing can tie" >&2
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
}

# Every command in cmd/ is deployed. The list used to be written out by hand
# here and again in .gitignore, and both copies drifted: cmd/reminder-coordinator
# was in neither, so scheduler job 32 ran a binary this script had never built
# and only a hand build ever updated it. Read the directory instead.
commands=()
for cmd_dir in "$REPO_DIR"/cmd/*/; do
  # An unmatched glob expands to itself, so check before trusting it.
  [ -d "$cmd_dir" ] || continue
  commands+=("$(basename "$cmd_dir")")
done

if [ ${#commands[@]} -eq 0 ]; then
  echo "ERROR: no commands found under $REPO_DIR/cmd"
  exit 1
fi

echo "==> Building ${#commands[@]} commands: ${commands[*]}"
mkdir -p "$BUILD_DIR"
for command_name in "${commands[@]}"; do
  echo "==> Building $command_name..."
  go build -o "$BUILD_DIR/$command_name" "./cmd/$command_name"
  echo "    built: $(ls -lh "$BUILD_DIR/$command_name" | awk '{print $5}')"
  check_provenance "$BUILD_DIR/$command_name"
done

# The new binary refuses to start on an undeclared SCHEDULER_PORT*, SCHEDULER_DB*
# or SCHEDULER_TOKEN* variable, and a scheduler that does not start runs no job
# on the box. So ask before the stop, against the running service's own
# environment. The test prints a verdict, never a value.
echo "==> Checking the running service's environment against the declared settings..."
live_pid="$(systemctl --user show -p MainPID --value "$SERVICE")"
if [ -n "$live_pid" ] && [ "$live_pid" != "0" ]; then
  go test -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' ./internal/config -args -live-environment-file="/proc/$live_pid/environ"
else
  echo "    $SERVICE is not running, so there is no environment to check"
fi

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binaries to $BIN_DIR..."
for command_name in "${commands[@]}"; do
  cp "$BUILD_DIR/$command_name" "$BIN_DIR/$command_name"
done

echo "==> Starting $SERVICE..."
systemctl --user daemon-reload
systemctl --user start "$SERVICE"

echo "==> Verifying..."
sleep 2
if systemctl --user is-active --quiet "$SERVICE"; then
  echo "    $SERVICE is running"
  journalctl --user -u "$SERVICE" -n 5 --no-pager 2>&1 | grep -v '^--'
else
  echo "ERROR: $SERVICE failed to start"
  journalctl --user -u "$SERVICE" -n 15 --no-pager 2>&1
  exit 1
fi

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
