#!/usr/bin/env bash
# Updates the Claude Code and Codex CLIs the bridge's harness wrappers exec
# into, then fails unless each installed version is the one npm calls
# latest. Scheduler job "nightly harness CLI update" runs it, and the
# harness_live tests check that the job exists and that its last run passed
# (internal/server/harness_cli_update_job_live_test.go).
#
# Both CLIs are npm global packages under mise's Node on this host. A CLI
# left behind shows a blocking update screen when it starts in a pty, and a
# keystroke sent into that screen answers it: on 2026-10-20 a live test's
# typing ran Codex's own self-update that way.
set -euo pipefail

export PATH="$HOME/.local/share/mise/shims:$PATH"

packages=(@anthropic-ai/claude-code @openai/codex)

installed_version() {
  npm ls -g --depth=0 --json "$1" | jq -r --arg p "$1" '.dependencies[$p].version // empty'
}

declare -A before
for package in "${packages[@]}"; do
  before[$package]="$(installed_version "$package")"
done

npm install -g "${packages[@]/%/@latest}"
mise reshim

status=0
for package in "${packages[@]}"; do
  latest="$(npm view "$package" version)"
  now="$(installed_version "$package")"
  echo "$package: ${before[$package]:-not installed} -> ${now:-not installed} (latest $latest)"
  if [ "$now" != "$latest" ]; then
    echo "ERROR: $package is $now after the update, not the latest $latest" >&2
    status=1
  fi
done
echo "claude --version: $(claude --version)"
echo "codex --version: $(codex --version)"
exit "$status"
