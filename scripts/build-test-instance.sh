#!/usr/bin/env bash
# Build everything scripts/test-instance.sh installs, into one folder:
#
#   scripts/build-test-instance.sh <output folder>
#
# The server, the runner and the mock harness come from this repo, with the
# front end built first so it is compiled into the server. log-store,
# tool-store, principal-store and grant-store come from their own repos beside
# this one, as they are checked out there.
#
# Every binary is built for linux/amd64 with no dependency on the system C
# library, so a folder built on one distribution runs on an older one: copy it
# and scripts/test-instance.sh to the machine, and run the script there.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PARENT_DIR="$(dirname "$REPO_DIR")"
OUT="${1:-}"
[ -n "$OUT" ] || { echo "usage: $0 <output folder>" >&2; exit 2; }
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

echo "==> Building the front end..."
"$REPO_DIR/scripts/build-ui.sh" >/dev/null

echo "==> Building the server, the runner and the mock harness from $(git -C "$REPO_DIR" rev-parse --short HEAD)..."
cd "$REPO_DIR"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/llm-bridge-server" ./cmd/llm-bridge-server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/llm-bridge-runner" ./runner
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/llm-bridge-mock" ./cmd/mock-harness

echo "==> Building log-store..."
(cd "$PARENT_DIR/log-store" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/log-store" ./cmd/log-store)

# These three use the SQLite driver that is C code, so they are linked whole
# (static) instead. principal-store's search needs SQLite's FTS5, as its own
# deploy script says.
build_static() { # <repo> <extra build tags>
  echo "==> Building $1..."
  (cd "$PARENT_DIR/$1" && CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build \
    -tags "netgo osusergo sqlite_omit_load_extension $2" \
    -ldflags '-linkmode external -extldflags "-static"' \
    -o "$OUT/$1" "./cmd/$1" 2>&1 | grep -v -E 'warning: Using|statically linked applications requires|: in function' || true)
  [ -x "$OUT/$1" ] || { echo "ERROR: $1 did not build" >&2; exit 1; }
}
build_static tool-store ""
build_static principal-store "sqlite_fts5"
build_static grant-store ""

cp "$REPO_DIR/scripts/test-instance.sh" "$OUT/test-instance.sh"
echo "==> Built into $OUT:"
ls -lh "$OUT" | awk 'NR>1 {print "    " $5, $9}'
