#!/usr/bin/env bash
# Cross-builds one harness wrapper for runner machines and publishes it where
# llm-bridge-server serves it from /api/runner/binary.
#
#   publish-harness-wrapper.sh <binary-name> [<go package>]
#
# Run it from the wrapper's repo, after that repo's deploy-gate check passed:
# each wrapper's deploy.sh calls it as its last step before the ledger record.
# <binary-name> is the file name the runner asks for (llm-bridge-<name>), and
# <go package> defaults to the repo root.
#
# Runner machines (SSDawn in WSL, a Mac) download llm-bridge-<name>-<os>-<arch>
# from the bridge. Until 2026-10-14 nothing published those files, so runners
# kept April builds; llm-bridge-claudecode's deploy.sh then grew its own copy of
# this loop (todo 10e10d90). This script replaces that copy, so the eleven
# wrappers share one, and cannot drift apart (todo 919fa8b7).
#
# Every target is built before any is installed, and each must carry HEAD as
# its vcs.revision: a build go stamped with nothing, or with another commit,
# reaches a remote machine with no record of what source it runs.
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  echo "usage: $0 <binary-name> [<go package>]" >&2
  exit 2
fi
BINARY_NAME="$1"
GO_PACKAGE="${2:-.}"

# The same lookup llm-bridge-server makes (internal/server/runner_assets.go,
# runnerAssetsDir): the environment variable when set, otherwise this path.
RUNNER_ASSETS_DIR="${LLMBRIDGE_RUNNER_ASSETS_DIR:-/usr/local/lib/llm-bridge-runner-binaries}"
TARGETS=("linux amd64" "linux arm64" "darwin amd64" "darwin arm64")

export PATH="$HOME/.local/share/mise/shims:$PATH"

HEAD_REVISION="$(git rev-parse HEAD)"

build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT

echo "==> Building $BINARY_NAME for runner machines at $HEAD_REVISION..."
for target in "${TARGETS[@]}"; do
  read -r target_os target_arch <<< "$target"
  build="$build_dir/$BINARY_NAME-$target_os-$target_arch"
  GOOS="$target_os" GOARCH="$target_arch" go build -o "$build" "$GO_PACKAGE"
  buildinfo="$(go version -m "$build")"
  build_revision="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
  build_modified="$(printf '%s\n' "$buildinfo" | awk -F= '$1 ~ /[[:space:]]vcs\.modified$/ {print $2}')"
  if [ "$build_revision" != "$HEAD_REVISION" ]; then
    echo "ERROR: the $target_os/$target_arch build carries vcs.revision '$build_revision', not HEAD $HEAD_REVISION." >&2
    echo "       go writes no stamp when it finds no .git directory (a git worktree has a .git file). Refusing to publish." >&2
    exit 1
  fi
  if [ "$build_modified" = "true" ]; then
    echo "ERROR: the $target_os/$target_arch build came from a dirty tree (vcs.modified=true). Refusing to publish." >&2
    exit 1
  fi
  echo "    built $target_os/$target_arch"
done

echo "==> Publishing to $RUNNER_ASSETS_DIR..."
if [ ! -d "$RUNNER_ASSETS_DIR" ]; then
  sudo mkdir -p "$RUNNER_ASSETS_DIR"
fi
for target in "${TARGETS[@]}"; do
  read -r target_os target_arch <<< "$target"
  sudo install -m 0755 "$build_dir/$BINARY_NAME-$target_os-$target_arch" "$RUNNER_ASSETS_DIR/$BINARY_NAME-$target_os-$target_arch"
  echo "    -> $RUNNER_ASSETS_DIR/$BINARY_NAME-$target_os-$target_arch"
done
echo "    runners pick it up at their next spawn (llm-bridge-runner refreshes a wrapper it downloaded when the bridge has a newer one)"
