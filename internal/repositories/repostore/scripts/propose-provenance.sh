#!/usr/bin/env bash
# Propose a provenance for every registered repository, from evidence on disk.
#
# `provenance` answers "is this ours?" and repo-store never infers it — the
# store records what it was told. This script is the other half: it reads the
# evidence, proposes an answer, and prints the evidence beside it so the
# proposal can be disagreed with. Without --apply it writes nothing.
#
# ## The evidence, and why origin is not it
#
# The obvious rule — "origin under the owner's org means ours" — is wrong when
# a vendored upstream is kept as a FORK under the same org: its `origin` is
# inside the org too. What separates it is a second remote:
#
#   our-service   origin github.com/example-owner/our-service
#   their-tool    origin github.com/example-owner/their-tool  upstream someone-else/their-tool
#
# So the rule is: a remote pointing OUTSIDE the owner's org is the evidence of
# vendored code, whatever that remote is called.
#
#   vendored — at least one remote outside the owner's org
#   ours     — at least one remote, all of them inside the owner's org
#   (none)   — no remotes at all: no evidence either way, so NO PROPOSAL
#
# The third bucket matters. A repository with no remote may well be your own
# work, but nothing this script can see says so. Proposing "ours" for it would
# be the guess the field exists to keep out of the numbers. Such repositories
# are listed for a hand answer instead.
#
# ## Exit codes
#
#   0 — every registered repository has a stored provenance, and none of them
#       contradicts the evidence
#   1 — something is unanswered or disagrees (details on stdout)
#   2 — could not run (missing tool, unreachable service, bad usage)
#
# ## Usage
#
#   scripts/propose-provenance.sh --owner ORG [ROOT]
#   scripts/propose-provenance.sh --owner ORG --apply [ROOT]
#   scripts/propose-provenance.sh --self-test
#
# --owner is required and has no default: which org counts as "us" is a fact
# about the host, not about this repository, and a default would bury it.
# REPO_STORE_URL overrides the service address, resolved otherwise from
# repo-store.service in this checkout.

set -euo pipefail
shopt -u dotglob

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"

for bin in curl jq git; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

OWNER=""
APPLY=0
SELFTEST=0
ROOT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --owner) OWNER="${2:-}"; shift 2 ;;
    --owner=*) OWNER="${1#*=}"; shift ;;
    --apply) APPLY=1; shift ;;
    --self-test) SELFTEST=1; shift ;;
    -h|--help) sed -n '2,50p' "$0"; exit 0 ;;
    -*) echo "ERROR: unknown option $1" >&2; exit 2 ;;
    *) ROOT="$1"; shift ;;
  esac
done

# --- the evidence -------------------------------------------------------------
#
# remote_org prints the org for one remote URL, for both forms git uses:
#   https://github.com/example-owner/x.git   and   git@github.com:example-owner/x.git
# A URL in neither form prints nothing, and a repository with such a remote is
# reported as unclassifiable rather than quietly treated as inside the org.

remote_org() {
  printf '%s\n' "$1" | sed -nE 's#^(git@|ssh://git@|https?://)([^:/]+)[:/]+([^/]+)/.*$#\3#p'
}

# classify prints "<verdict> <evidence>" for a repository directory.
classify() {
  local dir="$1" owner="$2" name url org
  local -a foreign=() unparsed=()
  local remotes
  remotes="$(git -C "$dir" remote 2>/dev/null || true)"
  if [ -z "$remotes" ]; then
    printf 'none no-remotes\n'
    return
  fi
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    url="$(git -C "$dir" remote get-url "$name" 2>/dev/null || true)"
    [ -n "$url" ] || continue
    org="$(remote_org "$url")"
    if [ -z "$org" ]; then
      unparsed+=("$name=$url")
    elif [ "$org" != "$owner" ]; then
      foreign+=("$name=$org")
    fi
  done <<< "$remotes"

  if [ ${#unparsed[@]} -gt 0 ]; then
    printf 'unparsable %s\n' "$(IFS=,; echo "${unparsed[*]}")"
    return
  fi
  if [ ${#foreign[@]} -gt 0 ]; then
    printf 'vendored %s\n' "$(IFS=,; echo "${foreign[*]}")"
    return
  fi
  printf 'ours all-remotes-under-%s\n' "$owner"
}

# --- self-test ----------------------------------------------------------------
#
# An instrument that has only ever reported one verdict has not been shown to be
# able to report another. This feeds classify one of each case and requires the
# exact answer, including the fork case that the naive origin rule gets wrong.

self_test() {
  SELFTEST_TMP="$(mktemp -d -t repo-store-provenance-selftest.XXXXXX)"
  trap "rm -rf '$SELFTEST_TMP'" EXIT
  local tmp="$SELFTEST_TMP" failed=0

  mk() { git init -q "$tmp/$1" && git -C "$tmp/$1" config user.email t@t && git -C "$tmp/$1" config user.name t; }

  mk ours-https;    git -C "$tmp/ours-https" remote add origin https://github.com/acme/ours-https.git
  mk ours-ssh;      git -C "$tmp/ours-ssh"   remote add origin git@github.com:acme/ours-ssh.git
  mk fork;          git -C "$tmp/fork"       remote add origin https://github.com/acme/fork.git
                    git -C "$tmp/fork"       remote add upstream https://github.com/otherorg/fork.git
  mk foreign-origin; git -C "$tmp/foreign-origin" remote add origin https://github.com/otherorg/x.git
  mk no-remote
  mk weird-url;     git -C "$tmp/weird-url"  remote add origin /some/local/path

  expect() {
    local dir="$1" want="$2" got
    got="$(classify "$tmp/$dir" acme | awk '{print $1}')"
    if [ "$got" != "$want" ]; then
      echo "SELF-TEST FAIL: $dir -> $got, want $want"; failed=1
    else
      echo "  ok  $dir -> $got"
    fi
  }
  expect ours-https ours
  expect ours-ssh ours
  # The case the naive rule gets wrong: origin is inside the org and the
  # repository is still somebody else's.
  expect fork vendored
  expect foreign-origin vendored
  expect no-remote none
  expect weird-url unparsable

  if [ "$failed" -ne 0 ]; then
    echo "self-test FAILED"; exit 1
  fi
  echo "self-test passed"
}

if [ "$SELFTEST" -eq 1 ]; then
  self_test
  exit 0
fi

if [ -z "$OWNER" ]; then
  echo "ERROR: --owner ORG is required — which org counts as 'us' is a fact about the host, and this script will not guess it" >&2
  exit 2
fi
[ -n "$ROOT" ] || ROOT="$HOME/repos"

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

BASE="$(resolve_base_url)"
ROWS="$(curl -sS --fail-with-body "$BASE/repos")" || {
  echo "ERROR: repo-store did not answer at $BASE" >&2; exit 2; }

if ! printf '%s' "$ROWS" | jq -e 'map(has("provenance")) | all' >/dev/null 2>&1; then
  echo "ERROR: $BASE serves rows with no provenance field — the running binary predates it." >&2
  echo "       Deploy repo-store, or point REPO_STORE_URL at an instance that has it." >&2
  exit 2
fi

echo "repo-store : $BASE"
echo "root       : $ROOT"
echo "owner org  : $OWNER"
echo

unanswered=0
disagreements=0
applied=0

while IFS=$'\t' read -r id name path stored; do
  [ -n "$name" ] || continue
  if [ -z "$path" ] || [ ! -d "$path/.git" ]; then
    printf '%-28s %-9s %s\n' "$name" "${stored:-unstated}" "no repository at its stored path — cannot judge"
    [ -z "$stored" ] && unanswered=$((unanswered+1))
    continue
  fi
  read -r verdict evidence <<< "$(classify "$path" "$OWNER")"

  case "$verdict" in
    ours|vendored)
      if [ "$stored" = "$verdict" ]; then
        continue
      fi
      if [ -n "$stored" ]; then
        printf 'DISAGREES %-28s stored=%-9s evidence says %s (%s)\n' "$name" "$stored" "$verdict" "$evidence"
        disagreements=$((disagreements+1))
        continue
      fi
      printf 'PROPOSE   %-28s %-9s %s\n' "$name" "$verdict" "$evidence"
      unanswered=$((unanswered+1))
      if [ "$APPLY" -eq 1 ]; then
        curl -sS --fail-with-body -X PUT "$BASE/repos/$id/provenance" \
          -H 'Content-Type: application/json' \
          -d "{\"provenance\":\"$verdict\"}" >/dev/null
        applied=$((applied+1))
      fi
      ;;
    none|unparsable)
      if [ -n "$stored" ]; then
        continue
      fi
      printf 'NO EVIDENCE %-26s %-9s %s — needs a hand answer\n' "$name" "unstated" "$evidence"
      unanswered=$((unanswered+1))
      ;;
  esac
done < <(printf '%s' "$ROWS" | jq -r '.[] | [(.id|tostring), .name, (.path // ""), (.provenance // "")] | @tsv')

echo
if [ "$APPLY" -eq 1 ]; then
  echo "applied $applied"
fi
if [ "$unanswered" -eq 0 ] && [ "$disagreements" -eq 0 ]; then
  echo "Every registered repository has a stored provenance and none contradicts the evidence."
  exit 0
fi
echo "unanswered: $unanswered   disagreements: $disagreements"
echo
echo "Apply the proposals with --apply. A NO EVIDENCE row is not a proposal —"
echo "state it by hand:"
echo
echo "  curl -sS -X PUT $BASE/repos/<id>/provenance \\"
echo "    -H 'Content-Type: application/json' -d '{\"provenance\":\"ours\"}'"
exit 1
