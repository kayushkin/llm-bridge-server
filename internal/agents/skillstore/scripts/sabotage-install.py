#!/usr/bin/env python3
"""Score skill-store's tests for the install path by breaking it.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-install.py [--diffs] [--crosstable]

⚠️ **This is the engine's FIFTH copy, and it is the same blob as the other
four.** md5 `9a81a32e5827b59c1a3093bf88187b17`, taken from git blob
`664f35f475edb9b7d018a28136211bf58a0ff53e` — what
`scheduler/fix/the-scorer-counts-occurrences-not-files`,
`bundle-store/docs/one-sabotage-engine-again`,
`agent-store/test/the-tracked-file-switch-is-unreached` and this repo's
`test/the-seed-profile-switch-is-unreached` all carry. Diff before editing; a
sixth blob is a fork. Take the blob off the BRANCH, not out of a working tree —
those checkouts sit on whatever branch their last pass left them on, and
md5summing them answers about the wrong commit (221st).

Why this seam is worth a scorer: measured on `main`, a `panic()` on the first
line of `Install`, `Uninstall`, `ListInstallations`, `copyTree`, `installSkill`,
`uninstallSkill` OR `listInstallations` left `go test ./...` green. Seven
functions, one mechanism, executed by nothing.

⚠️ **Read that measurement on `main`, not on the branch this file sits on.** The
222nd pass's seed-profile tests call `Install` and `ListInstallations` as
fixtures, so on this branch a `panic()` in either one reddens the suite — while
both remain entirely unasserted. The reach guard answers "somebody calls this",
which is a weaker claim than "somebody checks this", and a row whose function has
since been picked up as scaffolding will read as covered to the next census.

⚠️ And what this mechanism does to disk is why it matters past a coverage row.
`Install` derives its target by joining an ingested skill's frontmatter `name`
onto the install dir, and the FIRST thing it does with that path is
`os.RemoveAll`. Everything after that line is a write and nothing before it is a
containment check. The live install dir is `~/.claude/skills`.

SEVEN functions, three layers, and they can disagree:

    install.go  Install              removes the target, writes the files, records the row
    install.go  Uninstall            the only path that removes an installation
    install.go  ListInstallations    the only reader the seed manifest consults
    install.go  copyTree             carries the sibling files and their subdirectories
    server.go   installSkill         routes force in and maps two error kinds to 404/409
    server.go   uninstallSkill       turns "not installed" into a 200 rather than a failure
    server.go   listInstallations    the empty-array normalization dash depends on

📄 The 218th pass's rule — *assert the effect, not the reply* — is load-bearing
here in its sharpest form yet. `Install`'s reply is an `Installation` struct the
route serializes without re-reading the row, and its `InstalledAt` comes from a
SECOND `now()` call rather than from the value the INSERT wrote. The two agree
only because `now()` has one-second resolution.
`TheReplyDescribesTheRowThatWasActuallyWritten` pins them equal so the drift
cannot widen unseen.

📄 The 221st's *two effects, split the assertions* applies to every write case
below. `Install` puts a directory on disk AND a row in the database, and they
fail independently: files with no row can never be uninstalled, a row with no
files reads as installed to the seed manifest. Cases that break one and not the
other are the reason `WritesTheSkillToDiskAndRecordsTheRow` asserts both.

📄 The 222nd's *a row names the function the census could see, not the mechanism*
held again. The card's row said `Install`; the reach measurement found six more
unexecuted functions around it, including both other routes.

Case-writing rules inherited from the scheduler, bundle-store, agent-store and
seed-profile plans:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression.
  - A control is a case and obeys the case rules (220th).
  - Read every applied diff against its label (`--diffs`). A row prints the name
    you gave it, not the edit you made.
  - An UNNOTICED row is a claim about which LINE the engine moved before it is a
    claim about the tests (218th), and the case NAME is a claim the run can
    contradict too (221st).
  - Before reading UNNOTICED as a coverage hole, check whether the mutation is
    observable at all (221st).

⚠️ Needles here are anchored to their enclosing function wherever the fragment
repeats. `writeErr(w, 500, err.Error())` appears many times in server.go and
`os.MkdirAll(target, 0o755)` is one character from `os.MkdirAll(s.installDir,
0o755)` on the line above it.

⚠️ Region this plan does NOT cover, declared rather than left to be
rediscovered. Every case edits the install path, its two routes and the listing.
`Sync`, `UpsertSource`, the seed manifest, the version log and every skill or
source route are NOT sabotaged here — they are used as fixtures. The two
characterisations in `install_test.go` (the unchecked path segment and the
orphaned directory left by a rename) describe defects filed on noteboard, not
mechanisms this plan scores; sabotaging them would score a bug.

⚠️ `--crosstable` has NOT been taken for this repo and no `unreddened`
declaration is made below, because an empty one would read as "measured and
found none" rather than "not measured". The 222nd pass left that open and this
pass does not close it either.

Score when filed by the 223rd pass: 28/28 real mechanisms caught, both controls
behaved, exit 0. Two cases were rewritten mid-run rather than scored as gaps —
one was a compile error dressed as a verdict, and one was CAUGHT only because a
timestamp tie broke the right way (see its comment).
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "install.go", REPO / "server.go"]
PACKAGES = ["."]

CASES = [
    # ---- where the files go ----
    Case(
        "the target drops the skill name, so every skill installs over the same directory",
        [("\ttarget, err := s.installTargetForName(sk.Name)",
          "\ttarget, err := s.installDir, error(nil)")],
    ),
    Case(
        "the target is built from the skill id instead of its name, so nothing lands where the runner looks",
        [("\ttarget, err := s.installTargetForName(sk.Name)",
          '\ttarget, err := s.installTargetForName(fmt.Sprint(sk.ID))')],
    ),

    # ---- effect one: the files ----
    Case(
        "SKILL.md is written empty, so the installed skill has no instructions in it",
        [("\tif err := os.WriteFile(mdPath, []byte(sk.SkillMD), 0o644); err != nil {",
          "\tif err := os.WriteFile(mdPath, nil, 0o644); err != nil {")],
    ),
    Case(
        "SKILL.md is written from the skill's name rather than its body",
        [("os.WriteFile(mdPath, []byte(sk.SkillMD), 0o644)",
          "os.WriteFile(mdPath, []byte(sk.Name), 0o644)")],
    ),
    Case(
        "the sibling files are never copied, so a skill installs as its SKILL.md alone",
        [("\t\tif err := copyTree(srcDir, target); err != nil {\n\t\t\treturn nil, fmt.Errorf(\"copy files: %w\", err)\n\t\t}\n",
          "")],
    ),
    Case(
        "copyTree flattens subdirectories, so two files with one basename overwrite each other",
        [("\t\tdst := filepath.Join(dstDir, rel)\n\t\tif d.IsDir() {\n\t\t\treturn os.MkdirAll(dst, 0o755)\n\t\t}\n",
          "\t\tdst := filepath.Join(dstDir, filepath.Base(rel))\n\t\tif d.IsDir() {\n\t\t\treturn nil\n\t\t}\n")],
    ),
    Case(
        # The line that makes an install an install rather than a merge. Without
        # it the directory is the union of every version ever installed.
        "the target is no longer cleared, so files dropped by an earlier version outlive the upgrade",
        [("\tif err := os.RemoveAll(target); err != nil {\n\t\treturn nil, fmt.Errorf(\"clear target: %w\", err)\n\t}\n",
          "")],
    ),
    Case(
        # A skill shipping only a SKILL.md has no payload dir at all. Treating
        # that as an error makes the commonest kind of skill uninstallable.
        "a missing payload dir becomes an error, so a skill with no sibling files cannot install",
        [("\tif _, err := os.Stat(srcDir); err == nil {",
          "\tif _, err := os.Stat(srcDir); err != nil {")],
    ),

    # ---- effect two: the row ----
    Case(
        "the installation is never recorded, so the files are on disk and Uninstall cannot reach them",
        [("\t\tINSERT INTO installations (skill_id, target_path, installed_at)\n\t\tVALUES (?, ?, ?)\n\t\tON CONFLICT(skill_id) DO UPDATE SET target_path=excluded.target_path, installed_at=excluded.installed_at\n",
          "\t\tSELECT ?, ?, ? WHERE 0\n")],
    ),
    Case(
        "the row records the install dir rather than the skill's directory, so uninstalling one removes them all",
        [("\t\tON CONFLICT(skill_id) DO UPDATE SET target_path=excluded.target_path, installed_at=excluded.installed_at\n\t`, skillID, target, now())",
          "\t\tON CONFLICT(skill_id) DO UPDATE SET target_path=excluded.target_path, installed_at=excluded.installed_at\n\t`, skillID, s.installDir, now())")],
    ),
    Case(
        "reinstalling inserts a second row instead of updating, so target_path's UNIQUE constraint decides the outcome",
        [("\t\tON CONFLICT(skill_id) DO UPDATE SET target_path=excluded.target_path, installed_at=excluded.installed_at\n",
          "\t\tON CONFLICT(skill_id) DO NOTHING\n")],
    ),
    Case(
        # The reply is the only thing the route serializes, and it is not read
        # back from the row. Drift one field and nothing but an equality
        # assertion between reply and row can see it.
        "the reply reports a timestamp the row does not hold",
        [("\t\tTargetPath:  target,\n\t\tInstalledAt: now(),",
          "\t\tTargetPath:  target,\n\t\tInstalledAt: now() + 3600,")],
    ),
    Case(
        "the reply reports a target the row does not hold",
        [("\t\tTargetPath:  target,\n\t\tInstalledAt: now(),",
          "\t\tTargetPath:  target + \".pending\",\n\t\tInstalledAt: now(),")],
    ),

    # ---- the conflict gate ----
    Case(
        "the conflict check is dropped, so a second skill of the same name deletes the first and takes its place",
        [("\tif err == nil && existingSkillID != skillID {",
          "\tif false {")],
    ),
    Case(
        "the conflict check stops comparing ids, so a skill cannot be reinstalled over itself without force",
        [("\tif err == nil && existingSkillID != skillID {",
          "\tif err == nil {")],
    ),
    Case(
        "force is ignored, so an operator who asked to replace an installed skill is refused",
        [("\t\tif !force {\n\t\t\treturn nil, fmt.Errorf(\"%w: skill_id=%d owns %s\", ErrInstallConflict, existingSkillID, target)",
          "\t\tif true {\n\t\t\treturn nil, fmt.Errorf(\"%w: skill_id=%d owns %s\", ErrInstallConflict, existingSkillID, target)")],
    ),
    Case(
        "forcing skips the uninstall, so the displaced skill keeps a row pointing at a directory it no longer owns",
        [("\t\tif _, err := s.Uninstall(existingSkillID); err != nil {\n\t\t\treturn nil, fmt.Errorf(\"uninstall previous: %w\", err)\n\t\t}\n",
          "")],
    ),

    # ---- uninstall ----
    Case(
        "uninstall removes the row and leaves the files, so the skill is gone from the API and still on disk",
        [("\tif err := os.RemoveAll(target); err != nil {\n\t\treturn nil, fmt.Errorf(\"remove target: %w\", err)\n\t}\n",
          "")],
    ),
    Case(
        "uninstall removes the files and leaves the row, so the skill reads as installed with nothing behind it",
        [("\tif _, err := s.db.Exec(`DELETE FROM installations WHERE skill_id=?`, skillID); err != nil {",
          "\tif _, err := s.db.Exec(`SELECT ? `, skillID); err != nil {")],
    ),
    Case(
        "uninstalling something that was never installed becomes an error rather than a no-op",
        [("\t\tif errors.Is(err, sql.ErrNoRows) {\n\t\t\treturn nil, nil\n\t\t}\n",
          "\t\tif errors.Is(err, sql.ErrConnDone) {\n\t\t\treturn nil, nil\n\t\t}\n")],
    ),

    # ---- the listing the seed manifest reads ----
    Case(
        "the listing loses its source join, so two sources shipping one skill name are indistinguishable",
        [("\t\tSELECT i.skill_id, s.name, src.name, i.target_path, i.installed_at",
          "\t\tSELECT i.skill_id, s.name, s.name, i.target_path, i.installed_at")],
    ),
    Case(
        # ⚠️ Sorted by insertion rather than by time. `ORDER BY installed_at DESC`
        # is the likelier real drift, but every installation in a test is written
        # inside one second, so it ties and the row is decided by whichever order
        # SQLite happens to return — CAUGHT by luck, and luck that inverts if the
        # two writes ever straddle a second boundary. This edit separates the two
        # orders deterministically: the fixture installs zulu before alpha.
        "the listing is no longer ordered by name, so its output is insertion order",
        [("\t\tORDER BY s.name\n", "\t\tORDER BY i.skill_id\n")],
    ),

    # ---- the routes ----
    Case(
        "the install route forwards force unconditionally, so any caller displaces an installed skill",
        [("\tforce := r.URL.Query().Get(\"force\") == \"true\"", "\tforce := true")],
    ),
    Case(
        "the install route negates force, so asking to replace refuses and not asking replaces",
        [("\tforce := r.URL.Query().Get(\"force\") == \"true\"",
          "\tforce := r.URL.Query().Get(\"force\") != \"true\"")],
    ),
    Case(
        "a name collision is reported as a server error, so the caller cannot tell it to retry with force",
        [("\t\tif errors.Is(err, ErrInstallConflict) {\n\t\t\twriteErr(w, 409, err.Error())\n\t\t\treturn\n\t\t}\n",
          "")],
    ),
    Case(
        "a missing skill is reported as a server error rather than a 404",
        [("\tinst, err := h.s.Install(id, force)\n\tif err != nil {\n\t\tif errors.Is(err, ErrNotFound) {\n\t\t\twriteErr(w, 404, \"skill not found\")\n\t\t\treturn\n\t\t}\n",
          "\tinst, err := h.s.Install(id, force)\n\tif err != nil {\n")],
    ),
    Case(
        "the uninstall route reports not-installed for a skill it did uninstall",
        [("\tif inst == nil {\n\t\twriteJSON(w, 200, map[string]string{\"status\": \"not-installed\"})\n\t\treturn\n\t}\n\twriteJSON(w, 200, inst)",
          "\twriteJSON(w, 200, map[string]string{\"status\": \"not-installed\"})\n\t_ = inst")],
    ),
    Case(
        "the empty listing serializes as null, which is not an array and cannot be mapped over",
        [("\tif insts == nil {\n\t\tinsts = []Installation{}\n\t}\n", "")],
    ),

    # ---- controls ----
    # Known-positive: the target path is what every effect in this mechanism is
    # addressed to, so drifting it has to redden. Drifts the value rather than
    # deleting the line, per the case rules.
    Case(
        "CONTROL known-positive: every install lands one directory deeper than it reports",
        [("\ttarget := filepath.Clean(filepath.Join(installDir, name))",
          "\ttarget := filepath.Clean(filepath.Join(installDir, name, \"nested\"))")],
    ),
    # Known-negative: the wording of the conflict error. The suite matches it
    # with errors.Is and asserts the 409, never the sentence.
    Case(
        "CONTROL known-negative: the conflict error's sentence is reworded",
        [('return nil, fmt.Errorf("%w: skill_id=%d owns %s", ErrInstallConflict, existingSkillID, target)',
          'return nil, fmt.Errorf("%w: skill %d already owns %s", ErrInstallConflict, existingSkillID, target)')],
        expected_unnoticed="the suite matches this error with errors.Is and asserts the 409, never its text",
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
