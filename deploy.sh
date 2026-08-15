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

# Checked BEFORE the install, for the same reason the smokes are: an unidentifiable
# binary compiles perfectly and reads clean in the log, so installing first would put
# it in front of live sessions and only then tell us it cannot be traced to a commit.
echo "==> Checking provenance..."
buildinfo="$(go version -m /tmp/mailstack-build)"
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
