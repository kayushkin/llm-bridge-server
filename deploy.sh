#!/usr/bin/env bash
set -euo pipefail
eval "$("$HOME/.local/bin/mise" activate bash)"

BIN="$HOME/bin/mailstack"

cd "$(dirname "$0")"

if ! git diff-index --quiet HEAD -- 2>/dev/null; then
    echo "==> Refusing to deploy: uncommitted changes in $(pwd)"
    git status --short
    exit 1
fi

echo "==> Compiling mailstack..."
go build -o /tmp/mailstack-build ./cmd/mailstack/

echo "==> Pushing..."
git push

echo "==> Deploying..."
systemctl --user stop mailstack.service 2>/dev/null || true
cp /tmp/mailstack-build "$BIN"
systemctl --user start mailstack.service

echo "==> Verifying..."
sleep 2
systemctl --user is-active mailstack.service

echo "==> Done."
