#!/usr/bin/env python3
"""Score bundle-store's tests for the `enabled` switch by breaking it on purpose.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-enabled.py [--diffs] [--crosstable]

⚠️ **This is the engine's first copy outside scheduler.** It was landed there by
the 73rd pass and is meant to be the SAME BLOB everywhere it appears; the copy
in this repo is byte-identical at md5 `9d5ad6a0138c7f880f3f3c642166d80d`. If a
later pass finds these two disagreeing, they have forked, and the engine's own
header says what that costs. Diff before editing either.

Why this seam is worth a scorer: `enabled` is the only per-bundle switch the
service has, and until the 218th pass **nothing executed the code that writes
it**. A panic() at the first line of `Store.SetEnabled` or `handler.setEnabled`
left `go test ./...` green. The flag is written at two layers and read by a
third (`Resolve`, via `ListFilter.EnabledOnly`), so the mechanisms below
deliberately cross all three files rather than sitting in one.

THREE targets, and that is deliberate. This flag is not a property of one file:
the store writes it, the server routes it, and the resolver reads it. A scorer
covering only store.go would put a number on a third of the feature.

The service once seeded three bundles at every boot, and this file scored the
two lines that kept the seed from reverting an operator's flag. The seed is
gone, and so are those two cases. Run after the seed came out: 16/16 real
mechanisms caught, both controls behaved, exit 0.

Case-writing rules inherited from the scheduler plans:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression.
  - Read every applied diff against its label (`--diffs`). A row prints the name
    you gave it, not the edit you made.
  - An UNNOTICED row is not automatically a coverage hole. It can be a
    behavioral no-op, or a fake in the test file that swallows the value the
    mutation moved.

Score when filed by the 218th pass: 18/18 real mechanisms caught, both controls
behaved, exit 0.

⚠️ Two of those 18 exist because the FIRST run of this file scored a false
UNNOTICED. The needle for SetEnabled's RowsAffected branch also matched
DeleteBundle, the engine's uniqueness guard counted files rather than
occurrences, and the row scored the wrong function while reading as a coverage
hole in the right one. Fixing the engine turned that row CAUGHT and exposed a
genuine unpinned branch in DeleteBundle, now its own case and its own test. An
UNNOTICED row is a claim about which line moved — check that before believing
it is a claim about the tests.

⚠️ `--crosstable` has NOT been taken for this repo, and no `unreddened`
declaration is made below, because an empty one would read as "measured and
found none" rather than "not measured". The next pass on this repo owns that.
Expect it to be large and mostly silent: `resolve_test.go` and `store_test.go`
predate this seam and cover code no case here touches, which is not a finding.

⚠️ Region this plan does NOT cover, declared rather than left to be
rediscovered. Every case below edits the enabled switch and the paths that read
it. `UpsertBundle`'s transaction, `resolveExtends`, `refuseInheritanceCycle`,
`DeleteBundle`'s ErrParentHasChildren guard, the member encode/decode helpers,
`memberIndex.lookup` and every route other than enable/disable are NOT sabotaged
here. That is the seam the 218th pass opened and nothing more; it is not a
statement that the rest is covered.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [
    REPO / "store.go",
    REPO / "server.go",
    REPO / "resolve.go",
]
PACKAGES = ["./"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: SetEnabled writes the opposite of what it was asked",
        [("SET enabled=?, updated_at=? WHERE id=?", "SET enabled=NOT ?, updated_at=? WHERE id=?")],
    ),
    Case(
        "CONTROL known-negative: reword the comment above SetEnabled's sibling helper",
        [("// ListFilter narrows ListBundles.", "// ListFilter is how a caller narrows what ListBundles returns.")],
        expected_unnoticed="a prose comment. Nothing compiles against it and no test reads it. "
                           "This control is what proves the rows below redden because of the "
                           "behavior they move, not because any edit to these files reddens.",
    ),

    # ---- Store.SetEnabled: the four decisions in four lines ----
    Case(
        "the UPDATE targets a different column, so the flag never moves",
        [("SET enabled=?, updated_at=? WHERE id=?", "SET effort=?, updated_at=? WHERE id=?")],
    ),
    Case(
        "the UPDATE loses its WHERE, so one disable disables every bundle",
        [("SET enabled=?, updated_at=? WHERE id=?", "SET enabled=?, updated_at=? WHERE id=id")],
    ),
    Case(
        "updated_at is not stamped, so a flag change leaves no trace it happened",
        [("SET enabled=?, updated_at=? WHERE id=?", "SET enabled=?, updated_at=updated_at WHERE id=?")],
    ),
    Case(
        "a zero-row UPDATE reports success, so disabling a bundle that does not exist is a 200",
        # Drifted rather than deleted: removing the block orphans `res` and the
        # row scores `compile error` instead of a verdict.
        #
        # ⚠️ The needle carries the Exec line above it for a reason. The bare
        # `if n, _ := res.RowsAffected(); n == 0 {` appears TWICE in store.go —
        # here and in DeleteBundle — and the engine's uniqueness guard counts
        # FILES, not occurrences, so it sabotaged DeleteBundle and this row
        # scored a false UNNOTICED. See sabotage.py's `_apply_case`, fixed by
        # the 218th pass; a copy of the engine that still lets this through will
        # give you the same wrong answer.
        [("`, boolToInt(enabled), now(), id)\n\tif err != nil {\n\t\treturn err\n\t}\n"
          "\tif n, _ := res.RowsAffected(); n == 0 {",
          "`, boolToInt(enabled), now(), id)\n\tif err != nil {\n\t\treturn err\n\t}\n"
          "\tif n, _ := res.RowsAffected(); n < 0 {")],
    ),
    Case(
        # Found by accident: the mis-aimed needle above sabotaged this branch
        # instead and scored UNNOTICED, which was a true verdict about the wrong
        # function. DeleteBundle's not-found path is outside this plan's declared
        # seam, but a hole this file has already proved is not one to leave
        # silent, so it is pinned rather than declared.
        "DeleteBundle reports success for a bundle that does not exist",
        [("FOREIGN KEY constraint failed\") {\n\t\t\treturn fmt.Errorf(\"%w: %v\", ErrParentHasChildren, err)\n"
          "\t\t}\n\t\treturn err\n\t}\n\tif n, _ := res.RowsAffected(); n == 0 {",
          "FOREIGN KEY constraint failed\") {\n\t\t\treturn fmt.Errorf(\"%w: %v\", ErrParentHasChildren, err)\n"
          "\t\t}\n\t\treturn err\n\t}\n\tif n, _ := res.RowsAffected(); n < 0 {")],
    ),
    Case(
        "boolToInt is inverted, so every enable is a disable",
        [("func boolToInt(b bool) int {\n\tif b {", "func boolToInt(b bool) int {\n\tif !b {")],
    ),

    # ---- the read side: EnabledOnly and Resolve ----
    Case(
        "EnabledOnly stops filtering, so a disabled bundle still resolves",
        [("q += ` AND b.enabled = 1`", "q += ` AND b.enabled >= 0`")],
    ),
    Case(
        "EnabledOnly inverts, so disabling a bundle is the only way to use it",
        [("q += ` AND b.enabled = 1`", "q += ` AND b.enabled = 0`")],
    ),
    Case(
        "scanBundle reads the flag as always-on, so nothing ever reports itself disabled",
        [("b.Enabled = enabled == 1", "b.Enabled = true")],
    ),
    Case(
        "Resolve stops asking for enabled bundles only",
        [("s.ListBundles(ListFilter{EnabledOnly: true})", "s.ListBundles(ListFilter{})")],
    ),

    # ---- handler.setEnabled and the route pair ----
    Case(
        "the disable route forwards true — the copy-paste that leaves one verb doing nothing",
        [("func (h *handler) disableBundle(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, false) }",
          "func (h *handler) disableBundle(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, true) }")],
    ),
    Case(
        "the enable route forwards false",
        [("func (h *handler) enableBundle(w http.ResponseWriter, r *http.Request)  { h.setEnabled(w, r, true) }",
          "func (h *handler) enableBundle(w http.ResponseWriter, r *http.Request)  { h.setEnabled(w, r, false) }")],
    ),
    Case(
        "the handler replies 200 without writing anything — the failure the reply cannot show",
        [("\tif err := h.s.SetEnabled(id, v); err != nil {\n\t\trespondLookup(w, err)\n\t\treturn\n\t}\n", "")],
    ),
    Case(
        "a store error is swallowed and reported as success",
        [("\tif err := h.s.SetEnabled(id, v); err != nil {\n\t\trespondLookup(w, err)\n\t\treturn\n\t}",
          "\t_ = h.s.SetEnabled(id, v)")],
    ),
    Case(
        "the reply names an id the handler invented rather than the one it wrote",
        [('writeJSON(w, 200, map[string]any{"id": id, "enabled": v})',
          'writeJSON(w, 200, map[string]any{"id": int64(1), "enabled": v})')],
    ),
    Case(
        "parseID accepts 0 and negatives, so /bundles/0/disable is a lookup rather than a 400",
        [("if err != nil || id <= 0 {", "if err != nil {")],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
