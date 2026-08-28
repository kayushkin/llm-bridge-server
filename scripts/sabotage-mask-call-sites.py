#!/usr/bin/env python3
"""Score whether anything notices when `aiauth status` stops masking a credential.

`MaskKey` lives in the root package and is pinned there by `TestMaskKey`. That test
holds the *helper*. It cannot hold the three *call sites* in `cmd/aiauth/main.go`,
which are the lines that decide whether a real secret reaches the terminal.

Each arm rewrites one line in the working tree, runs the suite, restores the file, and
records whether the suite went red. An arm marked `must_redden` that survives is a call
site nothing is watching.

Contract (see ~/.nightly-shared/README.md):

    sabotage-mask-call-sites.py                    score every arm
    sabotage-mask-call-sites.py --list-sabotages   one arm name per line, exit 0
    sabotage-mask-call-sites.py --arm <name>       score one arm
    <anything else>                                exit 2

The rig refuses rather than guessing when the tree is dirty, the baseline is red, an
anchor is missing, or an anchor matches more than once. A needle that matches nothing
scores as SKIPPED; a needle that matches the wrong one of two identical lines prints
green and is indistinguishable from a control that behaved, so ambiguity is a refusal.
"""

import argparse
import json
import pathlib
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
MAIN = "cmd/aiauth/main.go"
RESOLVE = "resolve.go"

# name -> (relative file, needle, replacement, must_redden, why)
ARMS = {
    "oauth-site-unmasked": (
        MAIN,
        "masked = aiauth.MaskKey(c.Access)",
        "masked = c.Access",
        True,
        "the oauth access token reaches the terminal in full",
    ),
    "token-site-unmasked": (
        MAIN,
        "masked = aiauth.MaskKey(c.Token)",
        "masked = c.Token",
        True,
        "the bearer token reaches the terminal in full",
    ),
    "apikey-site-unmasked": (
        MAIN,
        "masked = aiauth.MaskKey(c.Key)",
        "masked = c.Key",
        True,
        "the api key reaches the terminal in full",
    ),
    "masked-column-emptied": (
        MAIN,
        "name, c.Type, c.Provider, masked, status)",
        "name, c.Type, c.Provider, masked[:0], status)",
        True,
        "asserting only that the secret is ABSENT is satisfied by printing nothing",
    ),
    "helper-returns-raw-key": (
        RESOLVE,
        'return key[:4] + "..." + key[len(key)-4:]',
        "return key",
        True,
        "known-positive control: the helper's own test must catch this",
    ),
    "comment-only-crywolf": (
        MAIN,
        "// Sync to model-store (updates inber's credential DB)",
        "// cry-wolf control: an edit that changes no behaviour",
        False,
        "cry-wolf control: a green rig must stay green here",
    ),
}


def go(*a):
    p = subprocess.run(["go", *a], cwd=REPO, capture_output=True, text=True)
    return p.returncode, (p.stdout + p.stderr)


def run_suite():
    return go("test", "./...")


def builds_and_vets():
    """A mutated tree that does not compile — or that `go test`'s own vet pass would
    reject — reddens the suite for a reason that is not a test noticing anything.
    Detecting that by matching error prose is a guess; asking the toolchain is not."""
    code, out = go("build", "./...")
    if code != 0:
        return "mutated tree does not COMPILE", out
    code, out = go("vet", "./...")
    if code != 0:
        return "mutated tree does not VET (go test runs vet, so this reddens on its own)", out
    return None, ""


def git(*a):
    return subprocess.run(
        ["git", *a], cwd=REPO, capture_output=True, text=True
    ).stdout.strip()


def refuse(msg):
    print(f"REFUSED: {msg}", file=sys.stderr)
    sys.exit(2)


def score_arm(name):
    rel, needle, repl, must_redden, why = ARMS[name]
    path = REPO / rel
    original = path.read_text()
    hits = original.count(needle)
    if hits == 0:
        return {"arm": name, "verdict": "SKIPPED", "reason": "anchor not found",
                "must_redden": must_redden, "why": why}
    if hits > 1:
        return {"arm": name, "verdict": "REFUSED", "reason": f"anchor matches {hits} times",
                "must_redden": must_redden, "why": why}
    try:
        path.write_text(original.replace(needle, repl))
        broken, detail = builds_and_vets()
        if broken:
            return {"arm": name, "verdict": "REFUSED",
                    "reason": broken + " — a build failure is not a caught bug",
                    "detail": detail.strip().splitlines()[-3:],
                    "must_redden": must_redden, "why": why}
        code, out = run_suite()
        reddened = code != 0
    finally:
        path.write_text(original)
    ok = reddened == must_redden
    return {
        "arm": name,
        "verdict": ("CAUGHT" if reddened else "SURVIVED") if must_redden
        else ("GREEN" if not reddened else "CRIED-WOLF"),
        "reddened": reddened,
        "must_redden": must_redden,
        "behaved": ok,
        "why": why,
    }


def main():
    ap = argparse.ArgumentParser(add_help=True)
    ap.add_argument("--list-sabotages", action="store_true")
    ap.add_argument("--arm")
    ap.add_argument("--json")
    args = ap.parse_args()

    if args.list_sabotages:
        for n in ARMS:
            print(n)
        return 0

    modified = git("status", "--porcelain", "--untracked-files=no")
    if modified:
        refuse("a TRACKED file is modified — restore it before scoring, or an arm "
               "restores the wrong text and the score is against a tree nobody chose\n"
               + modified)
    code, out = run_suite()
    if code != 0:
        refuse("BASELINE IS RED -- fix that before scoring any arm; a red baseline "
               "grades every arm as caught\n" + out[-2000:])

    names = [args.arm] if args.arm else list(ARMS)
    if args.arm and args.arm not in ARMS:
        refuse(f"unknown arm {args.arm!r}")

    rows = [score_arm(n) for n in names]
    behaved = sum(1 for r in rows if r.get("behaved"))
    for r in rows:
        mark = "ok " if r.get("behaved") else "XX "
        print(f"{mark}{r['arm']:<24} {r['verdict']:<10} {r['why']}")
    print(f"\nSCORE {behaved}/{len(rows)} arms behaved")

    result = {
        "repo": str(REPO),
        "branch": git("rev-parse", "--abbrev-ref", "HEAD"),
        "commit": git("rev-parse", "HEAD"),
        "tracked_modified": bool(git("status", "--porcelain", "--untracked-files=no")),
        "untracked": git("status", "--porcelain", "--untracked-files=all").splitlines(),
        "score": f"{behaved}/{len(rows)}",
        "rows": rows,
    }
    if args.json:
        pathlib.Path(args.json).write_text(json.dumps(result, indent=2))
    return 0 if behaved == len(rows) else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit:
        raise
    except Exception as e:  # noqa: BLE001
        refuse(f"{type(e).__name__}: {e}")
