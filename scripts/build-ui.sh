#!/usr/bin/env bash
# Build the front end that llm-bridge-server serves at /ui/, into ui/built/app,
# where the ui package embeds it at the next `go build`.
#
# Run this before building the server. A server compiled without it answers
# /ui/ with 503 user_interface_not_built.
#
# bridge-ui still reads generated types from sibling repos (../<store>/ts, as
# its package.json lists), so those checkouts must be present; scripts/bootstrap.sh
# clones the Go siblings only.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> Building chat-core..."
cd "$REPO_DIR/ui/chat-core"
npm ci --no-audit --no-fund

echo "==> Building bridge-ui for /ui/..."
cd "$REPO_DIR/ui/bridge-ui"
npm ci --no-audit --no-fund
npm run build:served-by-the-bridge

test -s "$REPO_DIR/ui/built/app/index.html" || {
  echo "ERROR: the build left no ui/built/app/index.html" >&2
  exit 1
}
echo "==> Front end built: $(find "$REPO_DIR/ui/built/app" -type f | wc -l) files in ui/built/app"
