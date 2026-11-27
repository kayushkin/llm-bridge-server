#!/usr/bin/env python3
"""Score agent-store's tests for the tracked-file enable switch by breaking it.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-tracked-file-enabled.py [--diffs] [--crosstable]

⚠️ **This is the engine's THIRD copy, and it is the same blob as the other two.**
md5 `9a81a32e5827b59c1a3093bf88187b17`, taken from git blob
`664f35f475edb9b7d018a28136211bf58a0ff53e`, which is what both
`scheduler/fix/the-scorer-counts-occurrences-not-files` and
`bundle-store/docs/one-sabotage-engine-again` carry after the 219th pass
re-unified them. Diff before editing; a fourth blob is a fork.

⚠️ And a measurement trap worth one line, because it nearly cost this pass a
false finding: the working checkouts of scheduler and bundle-store are on OTHER
branches, so `md5sum` over their working trees answers with two different old
blobs and reads exactly like the 219th's unification never happened. Compare the
BLOBS on the branches that carry the work, not the files in the checkout.

Why this seam is worth a scorer: `SetTrackedFileEnabled` is the only store
method on this box whose effect is a **rename on disk** as well as a row update,
and until the 221st pass nothing executed it — a panic() on its first line left
`go test ./...` green. Two effects is the whole point. Every other enable switch
here writes one row, so reading the row back proves the write happened; this one
can move the file without writing the row, or write the row without moving the
file, and the HTTP reply reports only the row. The cases below therefore split
"moved the file" from "wrote the row" rather than scoring them together.

TWO targets, deliberately. The flag is written in files.go and routed in
server.go by a one-line pair of handlers differing only by the bool they
forward. A scorer covering only files.go would put a number on the half of the
feature that cannot lose a verb to a copy-paste.

Case-writing rules inherited from the scheduler and bundle-store plans:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression.
  - A control is a case and obeys the case rules (220th). Both controls below
    were checked against them before the first full run.
  - Read every applied diff against its label (`--diffs`). A row prints the name
    you gave it, not the edit you made.
  - An UNNOTICED row is a claim about which LINE the engine moved before it is a
    claim about the tests (218th).

Score when filed by the 221st pass: 13/13 real mechanisms caught, both controls
behaved, exit 0.

⚠️ One of those 13 exists because the first full run scored a false-looking
UNNOTICED and the case's NAME was the thing that was wrong, not the tests. See
the comment on the no-op-guard case below: a mechanism can be dominated in every
path a reasonable test would exercise, and the row then reads as a coverage hole
in tests that are complete. The fix was one test driving the only separating
input, plus a case name that stopped claiming an effect the edit does not have.

⚠️ Region this plan does NOT cover, declared rather than left to be
rediscovered. Every case below edits the enable switch, `DiskPath`, and the two
routes that drive it. `Scan`, `upsertTrackedFile`, `WriteTrackedFileVersioned`,
`captureExistingIfNew`, the version log and every other route are NOT sabotaged
here — except where `TestAScanAgreesWithTheSuffixTheSwitchWrites` reaches Scan
on purpose, to pin the suffix as a contract between the two. That is the seam
the 221st pass opened and nothing more; it is not a statement that the rest is
covered.

⚠️ `--crosstable` has NOT been taken for this repo and no `unreddened`
declaration is made below, because an empty one would read as "measured and
found none" rather than "not measured". The next pass on this repo owns that.

📄 One case is missing on purpose. `SetTrackedFileEnabled` renames BEFORE it
updates the row, so a failing UPDATE leaves the file moved and the row stale,
and the caller is told the whole thing failed. There is no seam to inject a DB
failure through without a fake store, so the ordering is unscored rather than
silently claimed. It is the sharpest remaining hole in this function after the
clobber characterised in files_enabled_test.go.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "files.go", REPO / "server.go"]
PACKAGES = ["."]

CASES = [
    # ---- the guard that makes the switch idempotent ----
    Case(
        # ⚠️ This case was first named "...renames it to .disabled.disabled", which is
        # false, and it scored UNNOTICED against six tests that all believed it. When the
        # row already holds the requested state, `from` and `to` are the SAME path by
        # construction, so the body degrades to os.Rename(x, x) — a success that moves
        # nothing. The guard is dominated everywhere except on a file that is not on disk.
        "the no-op guard is dropped, so a redundant call stats a file it has no reason to need",
        [("if f.Enabled == enable {", "if false {")],
    ),
    Case(
        "the no-op guard is inverted, so the switch acts only when there is nothing to do",
        [("if f.Enabled == enable {", "if f.Enabled != enable {")],
    ),

    # ---- where the file goes ----
    Case(
        "the disabled suffix drifts, so a disabled file lands under a name no scan classifies",
        [('to = f.Path + ".disabled"', 'to = f.Path + ".off"')],
    ),
    Case(
        "enabling also targets the disabled path — the store-layer twin of a forwarded bool",
        [("\tto := f.Path\n", '\tto := f.Path + ".disabled"\n')],
    ),
    Case(
        "the file is never moved and only the row is written — the half the HTTP reply cannot show",
        [("if err := os.Rename(from, to); err != nil {", "if err := os.Rename(from, from); err != nil {"),
         ("if _, err := os.Stat(from); err != nil {", "_ = to\n\tif _, err := os.Stat(from); err != nil {")],
    ),
    Case(
        "the source is resolved from the canonical path rather than from where the row says the file is",
        [("from := f.DiskPath()", "from := f.Path")],
    ),

    # ---- what the row ends up saying ----
    Case(
        "the row is written with the value it already had, so the disk moves and the row does not follow",
        [("boolToInt(enable)", "boolToInt(f.Enabled)")],
    ),
    Case(
        "the pre-update row is returned instead of a fresh read, so the reply reports the old state",
        [("return s.GetTrackedFile(id)\n}", "return f, nil\n}")],
    ),
    Case(
        "a source file that is not on disk is reported as success with the untouched row",
        [('return nil, fmt.Errorf("source file missing: %s", from)', "return f, nil")],
    ),

    # ---- the route pair ----
    Case(
        "the disable route forwards true — the copy-paste that leaves one verb doing nothing",
        [("f, err := h.s.SetTrackedFileEnabled(id, false)", "f, err := h.s.SetTrackedFileEnabled(id, true)")],
    ),
    Case(
        "the enable route forwards false",
        [("f, err := h.s.SetTrackedFileEnabled(id, true)", "f, err := h.s.SetTrackedFileEnabled(id, false)")],
    ),
    Case(
        "the disable route swallows a store error and replies 200 with a nil file",
        [("\tf, err := h.s.SetTrackedFileEnabled(id, false)\n\tif err != nil {\n\t\twriteErr(w, 500, err.Error())\n\t\treturn\n\t}\n",
          "\tf, _ := h.s.SetTrackedFileEnabled(id, false)\n")],
    ),
    Case(
        "the enable route stops refusing an unparseable id and looks up file 0",
        [("func (h *handler) enableFile(w http.ResponseWriter, r *http.Request) {\n\tid, err := parseID(r)\n\tif err != nil {\n\t\twriteErr(w, 400, \"bad id\")\n\t\treturn\n\t}\n",
          "func (h *handler) enableFile(w http.ResponseWriter, r *http.Request) {\n\tid, _ := parseID(r)\n")],
    ),

    # ---- controls ----
    # Known-positive: DiskPath is how every reader in the package resolves a
    # tracked file to bytes, so inverting it has to redden the suite. Drifts the
    # condition rather than deleting the branch, per the case rules.
    Case(
        "CONTROL known-positive: DiskPath resolves every row to the wrong variant",
        [("if f.Enabled {\n\t\treturn f.Path\n\t}", "if !f.Enabled {\n\t\treturn f.Path\n\t}")],
    ),
    # Known-negative: the wording of an error nothing asserts on. A suite that
    # reports CAUGHT here is red for a reason that has nothing to do with the
    # mechanism, and the engine says so.
    Case(
        "CONTROL known-negative: the missing-source error is reworded",
        [('fmt.Errorf("source file missing: %s", from)', 'fmt.Errorf("source file absent: %s", from)')],
        expected_unnoticed="no test asserts the text of this error, only that there is one",
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
