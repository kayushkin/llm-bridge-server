#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed. It lives in
# healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="work-graph-store.service"
BINARY="work-graph-store"
HOOK_BINARY="work-graph-hook"
UNIT_SRC="$REPO_DIR/$SERVICE"
UNIT_DEST="$HOME/.config/systemd/user/$SERVICE"
# git's global core.hooksPath points here. Every hook name git knows gets a
# script, because a global hooksPath hides a repository's own .git/hooks from
# git; work-graph-hook runs the repository's own hook after recording.
HOOKS_DIR="$HOME/.local/share/work-graph-store/git-hooks"
HOOK_NAMES="applypatch-msg pre-applypatch post-applypatch pre-commit pre-merge-commit prepare-commit-msg commit-msg post-commit pre-rebase post-checkout post-merge pre-push pre-receive update proc-receive post-receive post-update reference-transaction push-to-checkout pre-auto-gc post-rewrite sendemail-validate post-index-change"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Testing..."
go test ./...

echo "==> Building $BINARY and $HOOK_BINARY..."
go build -o "$BINARY" ./cmd/work-graph-store
go build -o "$HOOK_BINARY" ./cmd/work-graph-hook

# A binary with no vcs.revision cannot be traced to a commit: go build writes
# none when .git is not a directory (a worktree), and does not fail.
echo "==> Checking provenance..."
vcs_revision="$(go version -m "$BINARY" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
if [ -z "$vcs_revision" ]; then
  echo "    REFUSING TO INSTALL: no vcs.revision in $BINARY. Build from the main clone." >&2
  exit 1
fi
echo "    vcs.revision=$vcs_revision"

# A set WORK_GRAPH_STORE_ variable that settings.go does not declare stops the
# new binary at boot. Ask before the old one is stopped.
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

echo "==> Installing binaries to $BIN_DIR..."
mkdir -p "$BIN_DIR"
cp "$BINARY" "$BIN_DIR/$BINARY"
# Every git command on the host may be running the hook right now: replace it
# by rename, so no git ever executes a half-copied file.
cp "$HOOK_BINARY" "$BIN_DIR/.$HOOK_BINARY.new"
mv -f "$BIN_DIR/.$HOOK_BINARY.new" "$BIN_DIR/$HOOK_BINARY"

echo "==> Installing git hook scripts in $HOOKS_DIR..."
mkdir -p "$HOOKS_DIR"
for name in $HOOK_NAMES; do
  # If the binary is gone, still run the repository's own hook, and never
  # fail: a failing reference-transaction hook aborts every commit.
  cat > "$HOOKS_DIR/.$name.new" <<SCRIPT
#!/bin/sh
# Written by work-graph-store's deploy.sh. See ~/repos/work-graph-store.
if [ -x "$BIN_DIR/$HOOK_BINARY" ]; then exec "$BIN_DIR/$HOOK_BINARY" $name "\$@"; fi
echo "work-graph-hook: $BIN_DIR/$HOOK_BINARY is missing; this git command was not recorded" >&2
own="\$(git rev-parse --path-format=absolute --git-common-dir)/hooks/$name"
if [ -x "\$own" ]; then exec "\$own" "\$@"; fi
exit 0
SCRIPT
  chmod 755 "$HOOKS_DIR/.$name.new"
  mv -f "$HOOKS_DIR/.$name.new" "$HOOKS_DIR/$name"
done

current_hooks_path="$(git config --global --get core.hooksPath || true)"
if [ -z "$current_hooks_path" ]; then
  git config --global core.hooksPath "$HOOKS_DIR"
  echo "    set git's global core.hooksPath to $HOOKS_DIR"
elif [ "$current_hooks_path" != "$HOOKS_DIR" ]; then
  echo "ERROR: git's global core.hooksPath is already $current_hooks_path; not replacing it" >&2
  exit 1
fi

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
ADDR="$(sed -n 's/^Environment=WORK_GRAPH_STORE_ADDR=//p' "$UNIT_SRC")"
[ -n "$ADDR" ] || { echo "ERROR: $UNIT_SRC sets no WORK_GRAPH_STORE_ADDR"; exit 1; }
curl -sfS "http://$ADDR/health" >/dev/null || { echo "ERROR: /health did not answer"; exit 1; }
curl -sfS "http://$ADDR/activity?format=text" >/dev/null || { echo "ERROR: /activity did not answer"; exit 1; }
echo "    $SERVICE is running and answering"

echo "==> Done."

# Last act: write this deploy to repo-store's ledger.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
