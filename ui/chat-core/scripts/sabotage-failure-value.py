#!/usr/bin/env python3
"""Apply one source mutation, run the suite, report whether it reddened.

Card 6bf13a5c's scoring rule, which is the whole point of the sweep:

    A. delete the mechanism the test names           -> often RED, looks pinned
    B. delete it AND make an earlier producer fire   -> GREEN is the defect

A mutation is a (file, old_text, new_text) triple. The runner refuses if
old_text is absent or appears more than once, so a mutation that silently
applied nowhere cannot be scored as "the tests caught it".

It also restores the file in a finally block, because a mutation left in the
tree is the next run's phantom failure.
"""

import json
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tree_hold  # noqa: E402  vendored; see tree_hold.py on keeping copies identical

# The repository this scorer belongs to, found from the scorer's own location.
# NOT an absolute path to wherever it was first written: a scorer pinned to a
# path outside its own checkout dies in `subprocess` the moment that directory
# goes away, and then exits non-zero for a reason that has nothing to do with
# the code it claims to be scoring.
ROOT = Path(__file__).resolve().parent.parent
PLAN_DIRECTORY = ROOT / "scripts" / "failure-value-plans"


def run_suite(test_files):
    command = ["npx", "vitest", "run", "--reporter=dot", *test_files]
    finished = subprocess.run(
        command, cwd=ROOT, capture_output=True, text=True, timeout=600
    )
    return finished.returncode, finished.stdout + finished.stderr


def apply_mutation(mutation):
    """Returns a restore callable. Raises if the mutation does not apply cleanly."""
    edits = mutation["edits"]
    originals = {}
    for edit in edits:
        path = ROOT / edit["file"]
        text = path.read_text()
        originals.setdefault(str(path), text)
    try:
        for edit in edits:
            path = ROOT / edit["file"]
            text = path.read_text()
            occurrences = text.count(edit["old"])
            if occurrences != 1:
                raise RuntimeError(
                    f"{edit['file']}: pattern occurs {occurrences} times, need exactly 1:\n"
                    f"  {edit['old'][:120]!r}"
                )
            path.write_text(text.replace(edit["old"], edit["new"]))
    except Exception:
        for filename, text in originals.items():
            Path(filename).write_text(text)
        raise

    def restore():
        for filename, text in originals.items():
            Path(filename).write_text(text)

    return restore


def score(mutation):
    restore = apply_mutation(mutation)
    try:
        code, output = run_suite(mutation["tests"])
    finally:
        restore()
    caught = code != 0
    failing = [
        line.strip()
        for line in output.splitlines()
        if line.strip().startswith(("×", "FAIL", "AssertionError"))
    ]
    return {
        "id": mutation["id"],
        "kind": mutation["kind"],
        "describes": mutation["describes"],
        "plan": mutation["plan"],
        "expected_unnoticed": mutation.get("expected_unnoticed"),
        "caught": caught,
        "exit": code,
        "failing": failing[:14],
    }


# A producer can be genuinely unobservable: `if (!candidate) return null` in
# webStorage.ts is followed by a read that throws on the same input, and the
# catch turns that throw into the identical null. No assertion over the return
# value can tell the two apart, so the mutation is UNNOTICED on a suite that
# pins the pair perfectly. Reporting that as a hole would leave the sweep unable
# to finish honestly, and silently exempting it would hide a real hole the day
# the code changes.
#
# So a mutation may DECLARE itself expected-unnoticed, with the reason. The
# declaration is a claim about the code, and a claim that stops being true is
# worse than no claim -- so a declared mutation that IS caught is reported as a
# problem in its own right, telling the reader to delete the declaration. That
# is the one thing an exemption list usually cannot do.
LABELS = {
    "CAUGHT": "CAUGHT            ",
    "UNNOTICED": "UNNOTICED         ",
    "EXPECTED-UNNOTICED": "EXPECTED-UNNOTICED",
    "STALE-DECLARATION": "STALE-DECLARATION ",
    "DID-NOT-APPLY": "DID-NOT-APPLY     ",
}


def classify(result):
    """The verdict on one mutation: a label, and whether it needs a human."""
    declared = result.get("expected_unnoticed")
    if result["caught"] is None:
        return "DID-NOT-APPLY"
    if result["caught"]:
        return "STALE-DECLARATION" if declared else "CAUGHT"
    return "EXPECTED-UNNOTICED" if declared else "UNNOTICED"


def problems(results):
    """Every result a run must not pass over, as ready-to-print lines.

    Controls are judged separately, per plan -- see `control_problems`.
    """
    lines = []
    for result in results:
        verdict = classify(result)
        if result["kind"] == "control":
            continue
        if verdict == "UNNOTICED":
            lines.append(f"UNNOTICED: {result['id']}: {result['describes']}")
        elif verdict == "DID-NOT-APPLY":
            lines.append(f"DID NOT APPLY: {result['id']}: {result['error'].splitlines()[0]}")
        elif verdict == "STALE-DECLARATION":
            lines.append(
                f"STALE DECLARATION: {result['id']} is declared expected_unnoticed "
                f"({result['expected_unnoticed']}) and the suite CAUGHT it. "
                "The declaration is out of date -- delete it."
            )
    return lines


def control_problems(results):
    """Controls are judged per plan, never pooled: each plan carries its own pair,
    and a plan whose controls misbehaved has measured nothing, so pooling would let
    one plan's healthy pair vouch for another plan's broken run."""
    lines = []
    for plan_name in sorted({r["plan"] for r in results}):
        controls = {
            r["id"].split("/", 1)[1]: r
            for r in results
            if r["plan"] == plan_name and r["kind"] == "control"
        }
        if not controls:
            lines.append(f"{plan_name}: no controls, so the run vouches for nothing")
            continue
        positive = controls.get("CONTROL-POSITIVE")
        if positive is None or positive["caught"] is not True:
            lines.append(f"{plan_name}: CONTROL-POSITIVE was not caught -- the suite never ran")
        negative = controls.get("CONTROL-NEGATIVE")
        if negative is None or negative["caught"] is not False:
            lines.append(
                f"{plan_name}: CONTROL-NEGATIVE was caught -- the suite is red for a reason "
                "unrelated to any mutation"
            )
    return lines


def load_plans(arguments):
    """Every mutation named on the command line, or the whole plan directory."""
    paths = [Path(a) for a in arguments] if arguments else sorted(PLAN_DIRECTORY.glob("*.json"))
    if not paths:
        raise SystemExit(f"no plans: {PLAN_DIRECTORY} is empty and none were named")
    plan = []
    for path in paths:
        if not path.is_absolute():
            path = (ROOT / path) if not path.exists() else path
        for mutation in json.loads(path.read_text()):
            mutation["plan"] = path.stem
            mutation["id"] = f"{path.stem}/{mutation['id']}"
            plan.append(mutation)
    return plan


def self_test():
    """Check the two properties the run's honesty rests on, in both directions.

    Neither needs vitest, and both are the parts that fail silently: a mutation
    that quietly applied nowhere would be scored as "the tests caught it", and a
    mutation left behind in the tree would redden the next run for no reason.
    """
    failures = []

    def check(description, actual, expected):
        if actual != expected:
            failures.append(f"{description}: expected {expected!r}, got {actual!r}")

    scratch = ROOT / "scripts" / ".self-test-scratch.txt"
    scratch.write_text("alpha\nbeta\nalpha\n")
    try:
        # Applies exactly once -> accepted, and restores afterwards.
        restore = apply_mutation({"edits": [{"file": str(scratch.relative_to(ROOT)), "old": "beta", "new": "GAMMA"}]})
        check("a unique pattern is applied", "GAMMA" in scratch.read_text(), True)
        restore()
        check("restore puts the file back", scratch.read_text(), "alpha\nbeta\nalpha\n")

        # Two occurrences -> refused, because which one was meant is unknown.
        for pattern, occurrences in (("alpha", 2), ("nowhere", 0)):
            try:
                apply_mutation({"edits": [{"file": str(scratch.relative_to(ROOT)), "old": pattern, "new": "X"}]})
                failures.append(f"a pattern occurring {occurrences} times was accepted")
            except RuntimeError:
                pass
            check(f"the tree is untouched after refusing {pattern!r}", scratch.read_text(), "alpha\nbeta\nalpha\n")

        # A multi-file mutation whose SECOND edit does not apply must roll the
        # first one back, or the run strands a half-applied mutation.
        other = ROOT / "scripts" / ".self-test-scratch-2.txt"
        other.write_text("delta\n")
        try:
            apply_mutation({"edits": [
                {"file": str(scratch.relative_to(ROOT)), "old": "beta", "new": "GAMMA"},
                {"file": str(other.relative_to(ROOT)), "old": "nowhere", "new": "X"},
            ]})
            failures.append("a half-applicable multi-file mutation was accepted")
        except RuntimeError:
            pass
        check("the first edit was rolled back", scratch.read_text(), "alpha\nbeta\nalpha\n")
        other.unlink()
    finally:
        scratch.unlink(missing_ok=True)

    # The verdict logic, in all four directions. An exemption that cannot go stale
    # is the failure mode this whole mechanism exists to avoid, so the direction
    # that matters most is the LAST one: a declared mutation that got caught must
    # be a problem, not a quiet pass.
    def result(kind, caught, declared=None):
        return {
            "id": "p/M",
            "plan": "p",
            "kind": kind,
            "describes": "d",
            "caught": caught,
            "expected_unnoticed": declared,
        }

    check("an undeclared catch is CAUGHT", classify(result("A", True)), "CAUGHT")
    check("an undeclared miss is UNNOTICED", classify(result("A", False)), "UNNOTICED")
    check(
        "a declared miss is EXPECTED-UNNOTICED",
        classify(result("A", False, "masked by the catch")),
        "EXPECTED-UNNOTICED",
    )
    check(
        "a declared CATCH is a stale declaration",
        classify(result("A", True, "masked by the catch")),
        "STALE-DECLARATION",
    )
    check("an undeclared catch needs nobody", problems([result("A", True)]), [])
    check(
        "a declared miss needs nobody",
        problems([result("A", False, "masked by the catch")]),
        [],
    )
    check("an undeclared miss needs a human", len(problems([result("A", False)])), 1)
    check(
        "a STALE declaration needs a human",
        len(problems([result("A", True, "masked by the catch")])),
        1,
    )
    # A control is judged by control_problems, never counted as a hole by problems().
    check("a control is not a hole", problems([result("control", False)]), [])

    for line in failures:
        print(f"SELF-TEST FAILED: {line}")
    if failures:
        return 1
    print("self-test passed: 16 checks, both directions")
    return 0


# A mutation is undone in a `finally`, which covers an exception and does NOT
# cover the process being killed -- a timeout, a Ctrl-C at the wrong moment, an
# OOM. Then the mutation stays on disk, looking exactly like source code, and
# every later run in that tree measures the sabotaged file as its baseline.
#
# That is not hypothetical. On 2026-08-15 a batch of these runs was killed by an
# outer 2-minute timeout while `permission-state`'s CONTROL-POSITIVE was applied.
# The next run scored `R1-no-prior-detail-A` as CAUGHT; on a restored tree the
# same mutation is UNNOTICED. The leftover mutation manufactured a PASS and hid a
# real hole -- the failure direction that gets a sweep marked finished.
#
# So: before touching anything, require every file the plans will edit to match
# HEAD. Only those files -- editing the tests is the normal way to close a hole
# found by this runner, and a dirty test tree is not a reason to refuse.
def refuse_a_dirty_source_tree(plan):
    files = sorted({edit["file"] for mutation in plan for edit in mutation["edits"]})
    finished = subprocess.run(
        ["git", "diff", "--name-only", "HEAD", "--", *files],
        cwd=ROOT, capture_output=True, text=True,
    )
    if finished.returncode != 0:
        raise SystemExit(
            "cannot check the source tree against HEAD:\n" + finished.stderr.strip()
        )
    dirty = [line for line in finished.stdout.splitlines() if line.strip()]
    if not dirty:
        return
    raise SystemExit(
        "REFUSING TO RUN: these files differ from HEAD and are files the plans mutate:\n"
        + "".join(f"    {name}\n" for name in dirty)
        + "\nA killed run leaves its mutation on disk, and scoring against it reports\n"
        "CAUGHT for holes that are really open. Restore them (git checkout -- <file>)\n"
        "and re-run. Pass --allow-dirty if you are deliberately scoring edited source."
    )


def main():
    """Take this tree exclusively, then score every mutation in the plan.

    Card `d869d2be`. A mutation's verdict here is read off the SUITE'S exit code, and
    the exit code belongs to the whole tree rather than to the mutation this run wrote.
    So a second run mutating the same files hands this one a red suite it did not cause,
    and this one records it as `caught` — the collision does not add noise, it
    **inflates the score**, and these scores are the numbers the nightly write-ups
    quote.

    ⚠️ The hold is taken HERE and not inside `score`, which is where the other five
    engines in this rollout take it. Their `score` is the whole run; this one's `score`
    is ONE mutation, called in a loop. Holding per mutation would release the tree
    between every pair of mutations — which is exactly the gap that makes
    `refuse_a_dirty_source_tree` blind to a concurrent run, since that guard also looks
    only at moments when the tree happens to be clean. The hazard is continuous, so the
    hold has to span the whole loop.

    `--self-test` is deliberately outside the hold: it mutates only its own scratch file
    under `scripts/`, touches nothing this scorer's plans touch, and is the thing you
    run to check the instrument — so it must not be refused merely because a real run is
    in progress.
    """
    if "--self-test" in sys.argv:
        return self_test()
    with tree_hold.exclusive_hold_on_tree(
            ROOT, purpose=os.path.basename(sys.argv[0] or "sabotage")) as refusal:
        if refusal:
            sys.exit("REFUSING: " + refusal)
        return score_every_mutation_on_a_held_tree()


def score_every_mutation_on_a_held_tree():
    """The scoring itself, on a tree this process already holds.

    Call `main` rather than this: on its own it will happily mutate a tree another run
    is mutating, which is exactly what the hold exists to stop.
    """
    allow_dirty = "--allow-dirty" in sys.argv
    plan = load_plans([a for a in sys.argv[1:] if a != "--allow-dirty"])
    if not allow_dirty:
        refuse_a_dirty_source_tree(plan)
    results = []
    for mutation in plan:
        try:
            result = score(mutation)
        except RuntimeError as error:
            result = {
                "id": mutation["id"],
                "plan": mutation["plan"],
                "kind": mutation["kind"],
                "describes": mutation["describes"],
                "caught": None,
                "error": str(error),
            }
        results.append(result)
        print(
            f"{LABELS[classify(result)]}  [{result['kind']}] {result['id']}: "
            f"{result['describes']}"
        )
        if result["caught"] is None:
            print(f"            {result['error'].splitlines()[0]}")
        sys.stdout.flush()

    Path(ROOT / "mutation-results.json").write_text(json.dumps(results, indent=2))

    # A control is not a hole, and counting it as one is how a scorer ends up
    # reporting a defect on every clean run. CONTROL-NEGATIVE is SUPPOSED to go
    # unnoticed; CONTROL-POSITIVE is supposed to be caught. Both are statements
    # about whether the run measured anything, so they are judged, not tallied.
    real = [r for r in results if r["kind"] != "control"]
    tally = {}
    for result in real:
        verdict = classify(result)
        tally[verdict] = tally.get(verdict, 0) + 1
    print(
        f"\n{len(real)} real mutations: "
        + ", ".join(f"{count} {verdict}" for verdict, count in sorted(tally.items()))
    )

    complaints = problems(results) + control_problems(results)
    for line in complaints:
        print(line)
    if not complaints:
        plans = sorted({r["plan"] for r in results})
        print(f"controls behaved in all {len(plans)} plans, and no producer is unpinned")

    # Exit status answers "did this run measure a hole", not "did it like what it
    # found". A mutation that never applied, and a control that misbehaved, both
    # mean the run did not measure -- which is not the same as finding nothing,
    # but is equally not a pass. A DECLARED expected-unnoticed is neither: it is a
    # measurement that came out as the plan said it would.
    return 1 if complaints else 0


if __name__ == "__main__":
    sys.exit(main())
