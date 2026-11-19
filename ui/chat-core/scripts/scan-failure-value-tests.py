#!/usr/bin/env python3
"""Find TypeScript tests whose ONLY assertions are failure-value checks.

Card 6bf13a5c's shape, ported from go/ast to the TS half of the fleet:

    a test whose only assertion is that a function returned its failure value,
    where that value has more than one producer.

This scan answers the first clause only. The second clause ("more than one
producer") and the confirmation are the pair-of-mutations step, which no static
scan can do -- see the card.

Reports, per test function:
  * every matcher called on an expect(...)
  * whether all of them are failure-value matchers

Deliberately reports the negative direction too: a file with zero
failure-value-only tests is printed as a zero, so an empty result is
distinguishable from a scan that matched nothing.
"""

import json
import re
import sys
from pathlib import Path

# A matcher asserting "the thing under test produced its failure/empty value".
# toBe(true) is NOT here: asserting a success value is a different claim and
# pins a different thing. toThrow() with a pattern is here because the pattern
# almost always matches a status code, which many producers can emit.
FAILURE_VALUE_MATCHER = re.compile(
    r"""^(
          toBeNull
        | toBeUndefined
        | toBeFalsy
        | toThrow
        | toBeNaN
    )$""",
    re.VERBOSE,
)

FAILURE_VALUE_WITH_ARGUMENT = {
    "toBe": {"false", "null", "undefined", "0", '""', "''", "NaN"},
    "toEqual": {"[]", "{}", "null", "undefined"},
    "toHaveLength": {"0"},
    "toBeCloseTo": set(),
}

TEST_OPENER = re.compile(r"\b(?:it|test)(?:\.\w+)?\s*\(\s*(['\"`])(?P<name>(?:\\.|(?!\1).)*)\1")
EXPECT_CALL = re.compile(r"\bexpect(?:\.\w+)?\s*\(")


def matching_brace(text, open_index):
    """Index just past the block that opens at text[open_index] == '{'.

    Skips string literals, template literals and both comment forms, because a
    brace inside any of them is not a brace. Returns len(text) if unbalanced.
    """
    depth = 0
    index = open_index
    length = len(text)
    while index < length:
        character = text[index]
        if character in "\"'`":
            quote = character
            index += 1
            while index < length:
                if text[index] == "\\":
                    index += 2
                    continue
                if text[index] == quote:
                    break
                if quote == "`" and text.startswith("${", index):
                    # A template substitution can hold braces and quotes; walk it
                    # as a block so they do not leak into the brace count.
                    index = matching_brace(text, index + 1)
                    continue
                index += 1
        elif text.startswith("//", index):
            index = text.find("\n", index)
            if index == -1:
                return length
        elif text.startswith("/*", index):
            index = text.find("*/", index)
            if index == -1:
                return length
            index += 1
        elif character == "{":
            depth += 1
        elif character == "}":
            depth -= 1
            if depth == 0:
                return index + 1
        index += 1
    return length


def balanced_call_arguments(text, open_paren_index):
    """The text between a '(' and its partner, quote- and nest-aware."""
    end = matching_paren(text, open_paren_index)
    return text[open_paren_index + 1 : end - 1], end


def matching_paren(text, open_index):
    depth = 0
    index = open_index
    length = len(text)
    while index < length:
        character = text[index]
        if character in "\"'`":
            quote = character
            index += 1
            while index < length:
                if text[index] == "\\":
                    index += 2
                    continue
                if text[index] == quote:
                    break
                index += 1
        elif character == "(":
            depth += 1
        elif character == ")":
            depth -= 1
            if depth == 0:
                return index + 1
        index += 1
    return length


def matchers_in(block):
    """Every matcher invoked on an expect(...) inside this block.

    Returns a list of (matcher_name, argument_text). A chained
    .not/.resolves/.rejects is recorded on the name, because `.not.toBeNull()`
    is an assertion that something IS present -- the opposite claim.
    """
    found = []
    for expect_match in EXPECT_CALL.finditer(block):
        after = matching_paren(block, expect_match.end() - 1)
        tail = block[after:]
        chain_match = re.match(r"\s*((?:\.\s*\w+)+)\s*\(", tail)
        if not chain_match:
            found.append(("<no-matcher>", ""))
            continue
        chain = [part.strip() for part in chain_match.group(1).split(".") if part.strip()]
        open_paren = after + chain_match.end() - 1
        argument_text, _ = balanced_call_arguments(block, open_paren)
        matcher = ".".join(chain)
        found.append((matcher, argument_text.strip()))
    return found


def is_failure_value_assertion(matcher, argument_text):
    parts = matcher.split(".")
    if "not" in parts:
        # expect(x).not.toBeNull() asserts presence. Not this card's shape.
        return False
    leaf = parts[-1]
    if FAILURE_VALUE_MATCHER.match(leaf):
        return True
    if leaf in FAILURE_VALUE_WITH_ARGUMENT:
        return argument_text.replace(" ", "") in FAILURE_VALUE_WITH_ARGUMENT[leaf]
    return False


def scan_file(path):
    text = path.read_text()
    results = []
    for opener in TEST_OPENER.finditer(text):
        body_start = text.find("{", opener.end())
        if body_start == -1:
            continue
        body_end = matching_brace(text, body_start)
        block = text[body_start:body_end]
        matchers = matchers_in(block)
        if not matchers:
            continue
        failure_value = [is_failure_value_assertion(m, a) for m, a in matchers]
        results.append(
            {
                "file": str(path),
                "line": text.count("\n", 0, opener.start()) + 1,
                "name": opener.group("name"),
                "matchers": [{"matcher": m, "argument": a} for m, a in matchers],
                "assertions": len(matchers),
                "all_failure_value": all(failure_value),
            }
        )
    return results


def main():
    # Relative to this script's own repository, never to the caller's cwd.
    repository_root = Path(__file__).resolve().parent.parent
    root = Path(sys.argv[1]) if len(sys.argv) > 1 else repository_root / "test"
    every_test = []
    for path in sorted(root.rglob("*.test.ts")):
        every_test.extend(scan_file(path))

    flagged = [t for t in every_test if t["all_failure_value"]]
    by_file = {}
    for test in every_test:
        entry = by_file.setdefault(test["file"], {"tests": 0, "flagged": 0})
        entry["tests"] += 1
        entry["flagged"] += 1 if test["all_failure_value"] else 0

    print(f"{len(every_test)} tests with at least one assertion, across {len(by_file)} files")
    print(f"{len(flagged)} whose every assertion is a failure-value check\n")
    for filename in sorted(by_file):
        counts = by_file[filename]
        marker = " " if counts["flagged"] == 0 else "*"
        print(f"{marker} {counts['flagged']:>3} / {counts['tests']:<3} {filename}")
    print()
    for test in flagged:
        shapes = ", ".join(
            f"{m['matcher']}({m['argument']})" for m in test["matchers"]
        )
        print(f"{test['file']}:{test['line']}  {test['name']}")
        print(f"      {shapes}")

    (repository_root / "failure-value-tests.json").write_text(json.dumps(every_test, indent=2))


if __name__ == "__main__":
    main()
