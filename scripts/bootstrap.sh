#!/usr/bin/env bash
# Clone the sibling repos that llm-bridge-server depends on via go.mod
# `replace ../X` directives. Run once after a fresh `git clone`.
#
# Layout the server expects:
#
#   <parent>/
#     llm-bridge-server/   (this repo)
#     llm-bridge/
#     agent-store/
#     ...
#
# harness-store, hook-store, snapshot-store and log-store are no longer
# siblings: they live in this repo under internal/.
#
# Re-running is safe: existing checkouts are left alone (the script prints
# "exists" and continues). A sibling with no published GitHub remote is
# reported as missing.

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PARENT_DIR="$(dirname "$REPO_DIR")"

# Sibling name → git remote URL. Matches the `replace` block in go.mod.
declare -A SIBLINGS=(
  # --- core sibling Go modules (referenced by go.mod replace block) ---
  [llm-bridge]="https://github.com/kayushkin/llm-bridge.git"
  [agent-store]="https://github.com/kayushkin/agent-store.git"
  [bus]="https://github.com/kayushkin/bus.git"
  [memory-store]="https://github.com/kayushkin/memory-store.git"
  [aiauth]="https://github.com/kayushkin/aiauth.git"
  [model-store]="https://github.com/kayushkin/model-store.git"
)

# Order matters only for human-readable output.
ORDER=(
  llm-bridge agent-store bus memory-store
  aiauth model-store
)

echo "==> Bootstrapping sibling repos into $PARENT_DIR"

missing_optional=()
for name in "${ORDER[@]}"; do
  target="$PARENT_DIR/$name"
  remote="${SIBLINGS[$name]}"

  if [ -d "$target/.git" ]; then
    echo "    [exists] $name"
    continue
  fi

  if [ -z "$remote" ]; then
    echo "    [skip]   $name (no public remote known)"
    missing_optional+=("$name")
    continue
  fi

  echo "    [clone]  $name <- $remote"
  if ! git clone --quiet "$remote" "$target"; then
    echo "    [warn]   failed to clone $name; continuing"
    missing_optional+=("$name")
  fi
done

echo "==> Verifying build..."
cd "$REPO_DIR"
if ! go build -o /tmp/llm-bridge-bootstrap-check ./cmd/llm-bridge-server >/dev/null 2>&1; then
  echo "ERROR: go build failed after bootstrap. Run 'go build ./cmd/llm-bridge-server' to see details."
  exit 1
fi
rm -f /tmp/llm-bridge-bootstrap-check

echo "==> OK"
if [ ${#missing_optional[@]} -gt 0 ]; then
  echo
  echo "Missing optional siblings (server degrades gracefully): ${missing_optional[*]}"
fi
echo
echo "Next steps:"
echo "  - cp .env.example .env  &&  edit values to taste"
echo "  - ./scripts/e2e-smoke.sh        # mock-harness end-to-end smoke test"
echo "  - export POSTGRESTEST_ADMINISTRATOR_URL=\"\$(scripts/test-postgres.sh start)\"   # the storage tests need a Postgres"
echo "  - go test ./...                 # unit + conformance tests"
