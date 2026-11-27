#!/usr/bin/env python3
"""Score skill-store's tests for the per-machine seed profile switch by breaking it.

The scoring engine — and the rules it enforces as refusals — lives in
scripts/sabotage.py. This file is only the case list: one edit per mechanism
the suite is meant to pin.

    python3 scripts/sabotage-seed-profile.py [--diffs] [--crosstable]

⚠️ **This is the engine's FOURTH copy, and it is the same blob as the other
three.** md5 `9a81a32e5827b59c1a3093bf88187b17`, taken from git blob
`664f35f475edb9b7d018a28136211bf58a0ff53e` — what
`scheduler/fix/the-scorer-counts-occurrences-not-files`,
`bundle-store/docs/one-sabotage-engine-again` and
`agent-store/test/the-tracked-file-switch-is-unreached` all carry. Diff before
editing; a fifth blob is a fork. Take the blob off the BRANCH, not out of a
working tree — those checkouts sit on whatever branch their last pass left them
on, and md5summing them answers about the wrong commit (221st).

Why this seam is worth a scorer: until the 222nd pass, a `panic()` on the first
line of `SetSeedProfileEnabled`, `putSeedProfile`, `BuildSeedManifest` OR
`EnsureSeedProfile` left `go test ./...` green. The whole feature — all three
layers plus its default — was executed by nothing.

⚠️ And what OFF means here is why that matters more than a coverage row.
`BuildSeedManifest` returns an EMPTY manifest for a disabled machine, and an
empty manifest is not "nothing to do": it is the instruction the runner
reconciles to by DELETING every file it has already seeded. So this bool is the
difference between a machine keeping its skills and a machine having them
removed, and the failure is silent in both directions — a stuck-ON switch seeds
a machine that opted out, a stuck-OFF one wipes a machine that did not.

THREE layers, one feature, and they can disagree:

    seed.go     SetSeedProfileEnabled   writes the row
    server.go   putSeedProfile          routes the bool in
    seed.go     BuildSeedManifest       the only reader whose answer anyone acts on

A scorer covering only the store method would put a number on the layer least
able to lose the value, since the route forwards it through a struct field and
the manifest reads it back through a second Ensure call.

📄 The 218th pass's rule — *assert the effect, not the reply* — applies here in
its milder form and it is worth saying which. bundle-store's handler built its
200 from the bool it was CALLED with and never consulted the store, so the reply
was structurally incapable of reporting a failed write. This handler does
consult the store: `putSeedProfile` returns whatever `SetSeedProfileEnabled`
read back after its UPDATE. That makes the reply honest ABOUT THE ROW, and it is
still not the effect — the effect is the manifest. `TheRouteWritesTheValueItWasGiven`
therefore reads the store, and `DisablingOverHTTPEmptiesTheManifestServedOverHTTP`
reads the manifest, because a row written correctly and never consulted by the
consumer is the same outage as a row never written.

Case-writing rules inherited from the scheduler, bundle-store and agent-store plans:

  - Prefer a DRIFTED VALUE to a deletion. It orphans no variable, needs no
    second edit, and is the likelier real-world regression.
  - A control is a case and obeys the case rules (220th).
  - Read every applied diff against its label (`--diffs`). A row prints the name
    you gave it, not the edit you made.
  - An UNNOTICED row is a claim about which LINE the engine moved before it is a
    claim about the tests (218th), and the case NAME is a claim the run can
    contradict too (221st).

⚠️ Needles here are long on purpose. `writeErr(w, 400, "invalid json")` appears
THREE times in server.go, `machine_id required` three times, and
`return s.GetSeedProfile(machineID)` twice in seed.go — every one of them a
needle the engine would have refused, or worse, would have aimed at the wrong
handler had the occurrence guard not been added by the 218th. Each case below is
anchored to its enclosing function.

⚠️ Region this plan does NOT cover, declared rather than left to be
rediscovered. Every case edits the seed profile switch, the manifest's
enabled-gate, the $HOME containment check and the two routes that drive them.
`RecordObservedHash`, `postSeedObserve`, `postSeedDrift`, `getVersionContent`,
the version log, `Install`/`Uninstall` and every source/skill route are NOT
sabotaged here — `Install` is used as a fixture, not scored, and it is still an
open row on card e9b0b89c. This is the seam the 222nd pass opened and nothing
more; it is not a statement that the rest is covered.

⚠️ `--crosstable` has NOT been taken for this repo and no `unreddened`
declaration is made below, because an empty one would read as "measured and
found none" rather than "not measured". The next pass on this repo owns that.

Score when filed by the 222nd pass: 18/19 real mechanisms caught, the 19th
declared unnoticeable with its reason, both controls behaved, exit 0.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "seed.go", REPO / "server.go"]
PACKAGES = ["."]

CASES = [
    # ---- the bool the switch writes ----
    Case(
        "the bool-to-int mapping is inverted, so every request writes the opposite of what it asked for",
        [("\ten := 0\n\tif enabled {\n\t\ten = 1\n\t}\n", "\ten := 1\n\tif enabled {\n\t\ten = 0\n\t}\n")],
    ),
    Case(
        "the switch always writes ON, so opting a machine out silently does nothing",
        [("\ten := 0\n\tif enabled {\n\t\ten = 1\n\t}\n", "\ten := 1\n\t_ = enabled\n")],
    ),
    Case(
        "the switch always writes OFF, so a machine can be enabled and still have its files removed",
        [("\ten := 0\n\tif enabled {\n\t\ten = 1\n\t}\n", "\ten := 0\n\t_ = enabled\n")],
    ),

    # ---- which row it writes ----
    Case(
        "the UPDATE loses its machine filter, so one machine opting out disables the whole fleet",
        [("SET enabled=?, updated_at=? WHERE machine_id=?",
          "SET enabled=?, updated_at=? WHERE machine_id=? OR 1=1")],
    ),
    Case(
        "the row is no longer created first, so setting the switch on an unseen machine updates nothing",
        [("\tif _, err := s.EnsureSeedProfile(machineID); err != nil {\n\t\treturn nil, err\n\t}\n\ten := 0\n",
          "\ten := 0\n")],
    ),
    Case(
        # The 218th's shape: an answer assembled from the argument rather than
        # from storage. Here it takes two edits because the honest read is what
        # the function already does.
        "the write is skipped and the reply is assembled from the argument, so the caller is told it worked",
        [("\tif _, err := s.db.Exec(`UPDATE machine_seed_profiles SET enabled=?, updated_at=? WHERE machine_id=?`,\n\t\ten, now(), machineID); err != nil {\n\t\treturn nil, err\n\t}\n\treturn s.GetSeedProfile(machineID)\n}",
          "\t_ = en\n\tprof, err := s.GetSeedProfile(machineID)\n\tif prof != nil {\n\t\tprof.Enabled = enabled\n\t}\n\treturn prof, err\n}")],
    ),

    # ---- the default, and the read path that runs on every poll ----
    Case(
        "a machine with no profile defaults to NOT seeding, so a fresh runner deletes instead of installing",
        [("VALUES (?, 1, ?, ?)", "VALUES (?, 0, ?, ?)")],
    ),
    Case(
        # EnsureSeedProfile runs first on both read paths. Re-inserting rather
        # than returning the existing row makes the opt-out last until the next poll.
        "Ensure stops returning the existing row, so every poll re-enables a machine that opted out",
        [("\tprof, err := s.GetSeedProfile(machineID)\n\tif err == nil {\n\t\treturn prof, nil\n\t}\n",
          "\tprof, err := s.GetSeedProfile(machineID)\n\tif err == nil && !prof.Enabled {\n\t\treturn prof, nil\n\t}\n\t_ = prof\n")],
    ),
    Case(
        "Ensure stops refusing an empty machine id, so a request with no machine writes a nameless row",
        [('\tif machineID == "" {\n\t\treturn nil, fmt.Errorf("machine_id required")\n\t}\n', "")],
    ),

    # ---- the consumer: what OFF actually does ----
    Case(
        "the manifest ignores the opt-out, so a disabled machine is seeded anyway",
        [("\tif !prof.Enabled {\n\t\treturn mf, nil\n\t}\n", "\tif false {\n\t\treturn mf, nil\n\t}\n")],
    ),
    Case(
        "the manifest gate is inverted, so an ENABLED machine gets the empty manifest that deletes its files",
        [("\tif !prof.Enabled {\n\t\treturn mf, nil\n\t}\n", "\tif prof.Enabled {\n\t\treturn mf, nil\n\t}\n")],
    ),
    Case(
        "the manifest always claims enabled, so the runner cannot tell an opt-out from an empty library",
        [("\t\tEnabled:   prof.Enabled,\n", "\t\tEnabled:   true,\n")],
    ),
    Case(
        # ⚠️ This case was first named "...so the manifest emits an install_path it
        # cannot rewrite", which is FALSE, and it scored UNNOTICED against a test that
        # asserted exactly that. $HOME containment is checked TWICE: this outer check
        # skips the installation, and installPathRelative inside the walk drops the
        # entry. The inner one dominates the entry list completely — deleting this one
        # changes no manifest anyone can read. What it does change is that the walk now
        # RUNS, calling AppendSeedVersion on every file before the entry is discarded.
        # Measured: 0 entries either way, 1 spurious version row. The separating
        # observation is the version log, not the manifest.
        "the outer $HOME check is dropped, so an installation outside HOME is walked and version-logged before its entries are discarded",
        [("\tif !strings.HasPrefix(inst.TargetPath, home+string(os.PathSeparator)) {\n\t\treturn nil, nil\n\t}\n",
          "\tif false {\n\t\treturn nil, nil\n\t}\n")],
    ),
    Case(
        # ⚠️ Declared unnoticeable rather than left to score as a gap, because it is
        # dominated BY CONSTRUCTION and no test can separate it. WalkDir yields paths
        # prefixed by inst.TargetPath, and the outer check has already established
        # TargetPath starts with home+sep — so by the time installPathRelative runs,
        # HasPrefix(path, prefix) cannot be false. Reaching it means removing the outer
        # check, and the case above already scores that.
        #
        # The engine contradicts this declaration in BOTH directions, which is the point
        # of writing it down: if someone deletes the outer check, this row starts
        # reporting CAUGHT and the run says the declaration has expired.
        "the per-file $HOME prefix check is dropped",
        [("\tprefix := home + string(os.PathSeparator)\n\tif !strings.HasPrefix(path, prefix) {\n\t\treturn \"\", false\n\t}\n",
          "\tprefix := home + string(os.PathSeparator)\n\tif false {\n\t\treturn \"\", false\n\t}\n")],
        expected_unnoticed=("the outer check in snapshotInstallation guarantees it: every walked path "
                            "is under inst.TargetPath, which is already known to be under home"),
    ),
    Case(
        # The reachable half of the same function, and the one that decides what a
        # machine actually receives.
        "the dot-dot filter is dropped, so a file the store silently refuses to seed is seeded after all",
        [('\tif rel == "" || strings.Contains(rel, "..") {\n\t\treturn "", false\n\t}\n',
          '\tif rel == "" {\n\t\treturn "", false\n\t}\n')],
    ),

    # ---- the route ----
    Case(
        "the route forwards a constant, so every request through the API enables the machine",
        [("prof, err := h.s.SetSeedProfileEnabled(machineID, body.Enabled)",
          "prof, err := h.s.SetSeedProfileEnabled(machineID, true)")],
    ),
    Case(
        "the route forwards the negation — the copy-paste that leaves one verb doing the other's job",
        [("prof, err := h.s.SetSeedProfileEnabled(machineID, body.Enabled)",
          "prof, err := h.s.SetSeedProfileEnabled(machineID, !body.Enabled)")],
    ),
    Case(
        # Go's zero value for bool is the DESTRUCTIVE setting on this route, so a
        # swallowed decode error turns a malformed request into an opt-out.
        "the route swallows a decode error, so a truncated body reads as {\"enabled\":false}",
        [("\tif err := json.NewDecoder(r.Body).Decode(&body); err != nil {\n\t\twriteErr(w, 400, \"invalid json\")\n\t\treturn\n\t}\n\tprof, err := h.s.SetSeedProfileEnabled(machineID, body.Enabled)",
          "\t_ = json.NewDecoder(r.Body).Decode(&body)\n\tprof, err := h.s.SetSeedProfileEnabled(machineID, body.Enabled)")],
    ),
    Case(
        "the route stops requiring a machine id and configures the empty-string machine",
        [("func (h *handler) putSeedProfile(w http.ResponseWriter, r *http.Request) {\n\tmachineID := r.URL.Query().Get(\"machine_id\")\n\tif machineID == \"\" {\n\t\twriteErr(w, 400, \"machine_id required\")\n\t\treturn\n\t}\n",
          "func (h *handler) putSeedProfile(w http.ResponseWriter, r *http.Request) {\n\tmachineID := r.URL.Query().Get(\"machine_id\")\n")],
    ),

    # ---- controls ----
    # Known-positive: GetSeedProfile's decode is how every layer here learns the
    # bool, so inverting it has to redden. Drifts the comparison rather than
    # deleting the line, per the case rules.
    Case(
        "CONTROL known-positive: the stored bool is decoded inverted on every read",
        [("prof.Enabled = enabled == 1", "prof.Enabled = enabled != 1")],
    ),
    # Known-negative: the wording of a 400 nothing asserts on. A suite that
    # reports CAUGHT here is red for a reason unrelated to the mechanism.
    Case(
        "CONTROL known-negative: the route's missing-machine message is reworded",
        [("func (h *handler) putSeedProfile(w http.ResponseWriter, r *http.Request) {\n\tmachineID := r.URL.Query().Get(\"machine_id\")\n\tif machineID == \"\" {\n\t\twriteErr(w, 400, \"machine_id required\")\n",
          "func (h *handler) putSeedProfile(w http.ResponseWriter, r *http.Request) {\n\tmachineID := r.URL.Query().Get(\"machine_id\")\n\tif machineID == \"\" {\n\t\twriteErr(w, 400, \"machine_id is required\")\n")],
        expected_unnoticed="no test asserts the text of this 400, only its status code",
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
