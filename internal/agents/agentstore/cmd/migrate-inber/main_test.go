package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The tests in this file drive the migration loop, not textutil.UpperFirstRune.
//
// That distinction is the whole reason the file exists. internal/textutil
// already has a thorough suite, and it stayed green through both of the panics
// this package shipped, because neither of them was in the helper: one was in
// how the id reached it and the other was on the same line, one expression to
// the right. A helper test cannot see either.
//
// Every case starts from JSON bytes rather than a hand-built struct. agents.json
// is the tool's only input and both reachable panics are things the JSON parser
// will hand you without complaint — an empty object key, and a null value where
// a config object was expected. Constructing the struct directly in a test skips
// the step that produces them.

// parseAgentsFile is the first half of main(): bytes off disk to a parsed file.
func parseAgentsFile(t *testing.T, raw string) inberAgentsFile {
	t.Helper()
	var af inberAgentsFile
	if err := json.Unmarshal([]byte(raw), &af); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	return af
}

// planFor runs the derivation and indexes it by slug for easy assertions.
func planFor(t *testing.T, raw string) map[string]plannedAgent {
	t.Helper()
	planned, err := planMigration(parseAgentsFile(t, raw))
	if err != nil {
		t.Fatalf("planMigration: unexpected error: %v", err)
	}
	bySlug := make(map[string]plannedAgent, len(planned))
	for _, p := range planned {
		bySlug[p.Slug] = p
	}
	return bySlug
}

// TestPlanMigrationDerivesDisplayNamesFromIdsWithoutSplittingRunes is the
// regression test for `strings.ToUpper(id[:1]) + id[1:]`.
//
// The ascii case is a known-negative control: the old byte-indexed spelling got
// it right, so it must stay green when the fix is reverted. A suite where every
// case reddens under the sabotage is not telling the fix apart from the fixture.
func TestPlanMigrationDerivesDisplayNamesFromIdsWithoutSplittingRunes(t *testing.T) {
	cases := []struct {
		name      string
		id        string
		want      string
		leadWidth int // bytes in the id's first rune; 1 == the control
	}{
		{"ascii_control", "claxon", "Claxon", 1},
		{"two_byte_lead", "émile", "Émile", 2},
		{"three_byte_lead", "日本語", "日本語", 3},
		{"four_byte_lead", "🎯quest", "🎯quest", 4},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := utf8.RuneLen([]rune(c.id)[0]); got != c.leadWidth {
				t.Fatalf("fixture is wrong: id %q leads with a %d-byte rune, case declares %d",
					c.id, got, c.leadWidth)
			}

			raw, err := json.Marshal(map[string]any{
				"agents": map[string]any{c.id: map[string]any{}},
			})
			if err != nil {
				t.Fatalf("building fixture JSON: %v", err)
			}

			got := planFor(t, string(raw))[c.id].DisplayName

			if got != c.want {
				t.Errorf("display name for id %q = %q, want %q", c.id, got, c.want)
			}
			// Asserted separately from the equality above on purpose. A
			// mis-capitalised name and a name that is no longer valid UTF-8 are
			// different defects, and the byte cut produced the second one.
			if !utf8.ValidString(got) {
				t.Errorf("display name for id %q = % x, which is not valid UTF-8", c.id, got)
			}
		})
	}
}

// TestPlanMigrationOnAnEmptyAgentIdDoesNotPanic pins the louder half of the old
// spelling: ""[:1] is out of range.
//
// An empty object key is legal JSON and encoding/json stores it as an ordinary
// map entry, so reaching this needs nothing more exotic than a stray `"": {}`
// in a hand-edited agents.json.
func TestPlanMigrationOnAnEmptyAgentIdDoesNotPanic(t *testing.T) {
	planned, err := planMigration(parseAgentsFile(t, `{"agents": {"": {"role": "unnamed"}}}`))
	if err != nil {
		t.Fatalf("planMigration: unexpected error: %v", err)
	}
	if len(planned) != 1 {
		t.Fatalf("planned %d agents, want 1", len(planned))
	}
	if planned[0].DisplayName != "" {
		t.Errorf("display name for the empty id = %q, want %q", planned[0].DisplayName, "")
	}
	if planned[0].Role != "unnamed" {
		t.Errorf("role = %q, want %q — the rest of the entry must survive an empty id",
			planned[0].Role, "unnamed")
	}
}

// TestPlanMigrationRejectsANullAgentConfigInsteadOfPanicking pins the second
// panic on the line the fix touched.
//
// `{"agents": {"ghost": null}}` parses cleanly into a nil map value, and the
// migration loop then dereferenced it three times — Name, Role and Model. The
// rune fix repaired the id half of that line and left the dereference one
// expression to its right untouched.
func TestPlanMigrationRejectsANullAgentConfigInsteadOfPanicking(t *testing.T) {
	_, err := planMigration(parseAgentsFile(t, `{"agents": {"ghost": null, "claxon": {}}}`))
	if err == nil {
		t.Fatal("planMigration accepted a null agent config, want an error")
	}
	// The id has to be in the message: the whole point of refusing rather than
	// skipping is telling the operator which line of their file to fix.
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q does not name the offending agent id %q", err, "ghost")
	}
}

// TestPlanMigrationNamesEveryNullConfigNotJustTheFirst — a run that reports one
// bad entry at a time costs an edit-and-rerun cycle per entry, and map order
// makes which one you get random.
func TestPlanMigrationNamesEveryNullConfigNotJustTheFirst(t *testing.T) {
	_, err := planMigration(parseAgentsFile(t, `{"agents": {"ghost": null, "wraith": null}}`))
	if err == nil {
		t.Fatal("planMigration accepted null agent configs, want an error")
	}
	for _, id := range []string{"ghost", "wraith"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error %q does not name %q", err, id)
		}
	}
	// Sorted, for the same reason the plan itself is sorted: the ids come out of
	// a map, so an unsorted message names them in a different order on every run
	// and two runs over one broken file cannot be diffed.
	if strings.Index(err.Error(), "ghost") > strings.Index(err.Error(), "wraith") {
		t.Errorf("error %q names the null configs out of order, want them sorted", err)
	}
}

// TestPlanMigrationPrefersAConfiguredNameOverTheId pins the override that sits
// on top of the id fallback. Without this, deleting the fallback entirely would
// look like a passing change on any realistic agents.json, since every real
// entry has a name.
func TestPlanMigrationPrefersAConfiguredNameOverTheId(t *testing.T) {
	plan := planFor(t, `{"agents": {"claxon": {"name": "Claxon the Loud", "model": "opus"}}}`)

	if got := plan["claxon"].DisplayName; got != "Claxon the Loud" {
		t.Errorf("display name = %q, want the configured name %q", got, "Claxon the Loud")
	}
	if got := plan["claxon"].Model; got != "opus" {
		t.Errorf("model = %q, want %q", got, "opus")
	}
}

// TestPlanMigrationFallsBackToTheIdWhenTheConfiguredNameIsEmpty pins the
// direction of the override. cmd/seed gets this case wrong in the opposite
// direction — it lets an empty configured name blank the display name — and the
// two tools deriving the same field differently is filed separately.
func TestPlanMigrationFallsBackToTheIdWhenTheConfiguredNameIsEmpty(t *testing.T) {
	plan := planFor(t, `{"agents": {"claxon": {"name": "", "role": "herald"}}}`)

	if got := plan["claxon"].DisplayName; got != "Claxon" {
		t.Errorf("display name = %q, want the id fallback %q", got, "Claxon")
	}
}

// TestPlanMigrationOrdersAgentsBySlug pins the ordering.
//
// Ranging a map is randomised per run, so the tool used to migrate in a
// different order every time and its output could not be diffed against a
// previous run.
//
// Five entries, not three, because this test's job is to redden when the sort
// is removed and an unsorted map happens to come out in order anyway. At three
// entries that is one run in six — a flaky detector that reports a real hole as
// intermittent. At five it is one in 120.
func TestPlanMigrationOrdersAgentsBySlug(t *testing.T) {
	planned, err := planMigration(parseAgentsFile(t,
		`{"agents": {"oisin": {}, "brigid": {}, "manannan": {}, "claxon": {}, "scathach": {}}}`))
	if err != nil {
		t.Fatalf("planMigration: unexpected error: %v", err)
	}

	want := []string{"brigid", "claxon", "manannan", "oisin", "scathach"}
	if len(planned) != len(want) {
		t.Fatalf("planned %d agents, want %d", len(planned), len(want))
	}
	for i, slug := range want {
		if planned[i].Slug != slug {
			t.Errorf("agent %d = %q, want %q (full order: %v)", i, planned[i].Slug, slug, slugs(planned))
		}
	}
}

func slugs(planned []plannedAgent) []string {
	out := make([]string, len(planned))
	for i, p := range planned {
		out[i] = p.Slug
	}
	return out
}
