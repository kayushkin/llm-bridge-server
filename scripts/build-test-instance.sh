#!/usr/bin/env bash
# Build everything scripts/test-instance.sh installs, into one folder:
#
#   scripts/build-test-instance.sh <output folder>
#
# The server, the runner and the mock harness come from this repo, with the
# front end built first so it is compiled into the server; the server holds
# principal-store, grant-store, permission-store, tool-store, kanban-store,
# repo-store, file-store, reference-store, log-store and the other stores
# moved into it. No store is built from a sibling repo any more.
#
# The runner's disk-usage and computer-use helpers are left out on purpose. A
# runner in WSL installs them into the Windows user's own app data, where the
# machine's real runner keeps its copies. The test runner's PATH does not reach
# cmd.exe, so it cannot find that folder and offers neither (it logs an ERROR
# for each), and the test server offers no helper to fetch either way.
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

# The real harness wrappers. The machine supplies the agent programs
# themselves (claude, codex) and their logins; scripts/test-instance.sh
# use-machine-logins links them in.
for wrapper in llm-bridge-claudecode llm-bridge-codex; do
  echo "==> Building $wrapper..."
  (cd "$PARENT_DIR/$wrapper" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/$wrapper" .)
done


cp "$REPO_DIR/scripts/test-instance.sh" "$OUT/test-instance.sh"
echo "==> Built into $OUT:"
ls -lh "$OUT" | awk 'NR>1 {print "    " $5, $9}'
