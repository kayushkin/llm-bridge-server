#!/usr/bin/env bash
# Registry drift report for repo-store.
#
# Compares the rows repo-store serves against a walk of the repository root and
# reports both directions:
#
#   MISSING — a repository on disk that has no row, so it resolves with no tags
#   STALE   — a row whose stored path is not a repository directory
#
# Read-only. It never writes to the registry, and it is deliberately not
# scheduled: see "Why there is no red state" below.
#
# ## Why this exists
#
# There is no ingest anywhere — no walk in the service, no scan route, no cron
# job, no seed script in this repo. A row exists only because somebody sent
# POST /repos for it. A registry populated once by walking a directory drifts in
# two ways: it gets rows for directories that are not repositories, and it has
# no rows for repositories created afterwards.
#
# That matters beyond a wrong count because bundle-store's /resolve composes a
# session bundle from a repo's effective tags. A repository with no row has no
# tags, so it resolves to the empty-tag case silently.
#
# ## Why there is no red state
#
# This reports; it does not fail a nightly. Drift here is not a defect anyone
# introduced — it is the absence of an ingest, which is a known open question. A scheduled red light would fire every time a repo is
# created and say nothing new. Run it when you want the number, or after
# building the ingest to prove it works.
#
# ## Exit codes
#
#   0 — registry and filesystem agree
#   1 — drift found (details on stdout)
#   2 — could not run (missing tool, unreachable service, bad usage)
#
# ## Usage
#
#   scripts/registry-drift.sh [ROOT]      # default ROOT: ~/repos
#   scripts/registry-drift.sh --self-test # prove the walk before trusting it
#
# REPO_STORE_URL overrides the service address. Unset, the address is read from
# repo-store.service in this checkout — the file that actually defines how the
# service is deployed — rather than written as a literal here.

set -euo pipefail
shopt -u dotglob   # the walk must not descend into dotted directories

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"

for bin in curl jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

# --- the walk -----------------------------------------------------------------
#
# A directory is a repository if and only if it contains a `.git` DIRECTORY.
#
# Both halves of that test earn their place. Without any test, a plain directory
# counts — that is exactly how `memory` and `sessions` got rows. Testing only
# that `.git` exists is not enough either: a linked worktree's `.git` is a
# regular FILE pointing into its parent, so `~/repos/scheduler-wt-*` would count
# as three more repositories when they are three checkouts of one already
# counted. --self-test pins both.

walk_repositories() {
  local root="$1" dir
  for dir in "$root"/*/; do
    dir="${dir%/}"
    [ -d "$dir" ] || continue
    [ -d "$dir/.git" ] || continue
    printf '%s\n' "$dir"
  done | sort
}

# --- service address ----------------------------------------------------------

resolve_base_url() {
  if [ -n "${REPO_STORE_URL:-}" ]; then
    printf '%s\n' "${REPO_STORE_URL%/}"
    return
  fi
  local unit="$REPO_DIR/repo-store.service" addr
  if [ ! -f "$unit" ]; then
    echo "ERROR: $unit not found and REPO_STORE_URL is unset — cannot resolve the service address" >&2
    exit 2
  fi
  # Environment=REPO_STORE_ADDR=127.0.0.1:8306  ->  127.0.0.1:8306
  addr="$(sed -n 's/^Environment=REPO_STORE_ADDR=//p' "$unit" | tail -1)"
  if [ -z "$addr" ]; then
    echo "ERROR: $unit sets no REPO_STORE_ADDR — set REPO_STORE_URL explicitly" >&2
    exit 2
  fi
  case "$addr" in
    :*) printf 'http://127.0.0.1%s\n' "$addr" ;;   # ":8306" carries only the port
    *)  printf 'http://%s\n' "$addr" ;;            # "127.0.0.1:8306"
  esac
}

# --- self-test ----------------------------------------------------------------
#
# The 79th nightly pass's rule: an instrument that has only ever reported "clean"
# has not been shown to be able to report anything else. This feeds the walk a
# tree containing one of each thing it must judge and requires the exact answer.

self_test() {
  # Not `local`: the EXIT trap runs after this function has returned, so a
  # function-local would be unbound by then and `set -u` would turn a passing
  # self-test into a non-zero exit. The trap expands the path now, not later,
  # for the same reason.
  SELFTEST_TMP="$(mktemp -d -t repo-store-drift-selftest.XXXXXX)"
  trap "rm -rf '$SELFTEST_TMP'" EXIT
  local tmp="$SELFTEST_TMP"

  mkdir -p "$tmp/real-repo/.git"          # a repository        -> counted
  mkdir -p "$tmp/plain-directory"         # not a repository    -> skipped
  mkdir -p "$tmp/linked-worktree"
  echo "gitdir: $tmp/real-repo/.git/worktrees/wt" > "$tmp/linked-worktree/.git"
  mkdir -p "$tmp/.hidden-repo/.git"       # dotted              -> skipped

  local got want
  got="$(walk_repositories "$tmp")"
  want="$tmp/real-repo"

  if [ "$got" != "$want" ]; then
    echo "SELF-TEST FAILED" >&2
    echo "  expected exactly: $want" >&2
    echo "  got:" >&2
    printf '    %s\n' ${got:-<nothing>} >&2
    exit 1
  fi

  echo "self-test passed: the walk counts a repository, and skips a plain"
  echo "directory, a linked worktree (.git is a file) and a dotted directory."
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit 0
fi

# --- report -------------------------------------------------------------------

ROOT="${1:-$HOME/repos}"
if [ ! -d "$ROOT" ]; then
  echo "ERROR: root '$ROOT' is not a directory" >&2
  exit 2
fi
ROOT="$(cd "$ROOT" && pwd)"

BASE="$(resolve_base_url)"

TMP="$(mktemp -d -t repo-store-drift.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT

if ! curl -sS --max-time 15 "$BASE/repos?limit=100000" -o "$TMP/rows.json"; then
  echo "ERROR: repo-store unreachable at $BASE" >&2
  exit 2
fi
if ! jq -e 'type == "array"' "$TMP/rows.json" >/dev/null 2>&1; then
  echo "ERROR: $BASE/repos did not return a JSON array:" >&2
  head -c 400 "$TMP/rows.json" >&2
  echo >&2
  exit 2
fi

# Compare on PATH, not on name. Path is what identifies the thing on disk, and a
# name-based comparison would quietly agree whenever a row's name matches a
# directory whose path it does not actually point at.
jq -r '.[].path' "$TMP/rows.json" | sort > "$TMP/row_paths.txt"
jq -r '.[] | [.path, .name] | @tsv' "$TMP/rows.json" | sort > "$TMP/rows.tsv"
walk_repositories "$ROOT" > "$TMP/disk_paths.txt"

comm -23 "$TMP/disk_paths.txt" "$TMP/row_paths.txt" > "$TMP/missing.txt"

: > "$TMP/stale.txt"
while IFS=$'\t' read -r path name; do
  [ -n "$path" ] || continue
  if [ ! -d "$path/.git" ]; then
    reason="path is not a repository"
    [ -e "$path" ] || reason="path does not exist"
    printf '%s\t%s\t%s\n' "$name" "$path" "$reason" >> "$TMP/stale.txt"
  fi
done < "$TMP/rows.tsv"

rows_total="$(jq 'length' "$TMP/rows.json")"
disk_total="$(wc -l < "$TMP/disk_paths.txt" | tr -d ' ')"
missing_total="$(wc -l < "$TMP/missing.txt" | tr -d ' ')"
stale_total="$(wc -l < "$TMP/stale.txt" | tr -d ' ')"

echo "repo-store : $BASE"
echo "root       : $ROOT"
echo "rows       : $rows_total"
echo "on disk    : $disk_total"
echo

if [ "$missing_total" -gt 0 ]; then
  echo "MISSING ($missing_total) — a repository on disk with no row; it resolves with no tags:"
  while read -r path; do
    [ -n "$path" ] && echo "  $path"
  done < "$TMP/missing.txt"
  echo
fi

if [ "$stale_total" -gt 0 ]; then
  echo "STALE ($stale_total) — a row whose stored path is not a repository:"
  while IFS=$'\t' read -r name path reason; do
    [ -n "$name" ] && printf '  %-24s %s  (%s)\n' "$name" "$path" "$reason"
  done < "$TMP/stale.txt"
  echo
  echo "  Check what joins to a row before deleting it: this service's deploy"
  echo "  ledger and pull requests hang on its id, and other services may store it."
  echo
fi

if [ "$missing_total" -eq 0 ] && [ "$stale_total" -eq 0 ]; then
  echo "No drift: every repository under $ROOT has exactly one row, and every"
  echo "row points at a repository."
  exit 0
fi

echo "Drift found. There is no ingest to re-run — population is by"
echo "POST /repos, one repository at a time:"
echo
echo "  curl -sS -X POST $BASE/repos -H 'Content-Type: application/json' \\"
echo "    -d '{\"name\":\"<name>\",\"path\":\"<abs path>\"}'"
echo
echo "The service detects the tags from the path; do not hand-write them."
exit 1
