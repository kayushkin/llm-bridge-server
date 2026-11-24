package main

import (
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// These tests drive seed's identity derivation, not textutil.UpperFirstRune.
//
// The helper has its own suite and it was green for the whole time this file
// was capitalising a byte, because the helper was not the broken part. What
// follows exercises the call site: an id arriving from mapping.json, an
// override arriving from inber's agents.json, and the precedence between them.
//
// Fixtures are parsed from JSON rather than built as structs wherever the case
// is about what a config file can contain. An empty agent id is legal in
// mapping.json and is what made the old byte-indexed spelling panic.

// parseMapping is the first half of main() for mapping.json.
func parseMapping(t *testing.T, raw string) MappingFile {
	t.Helper()
	var mf MappingFile
	if err := json.Unmarshal([]byte(raw), &mf); err != nil {
		t.Fatalf("mapping fixture is not valid JSON: %v", err)
	}
	return mf
}

// parseInberConfig is the first half of main() for inber's agents.json.
func parseInberConfig(t *testing.T, raw string) InberConfig {
	t.Helper()
	var cfg InberConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("inber config fixture is not valid JSON: %v", err)
	}
	return cfg
}

// identityFor reproduces the lookup-then-resolve pair main()'s loop performs,
// so a test can pass in the two config files and get back what seed would store.
func identityFor(ma MappingAgent, inberCfg InberConfig) agentIdentity {
	var ia InberAgent
	hasInber := false
	if slug, pointsAtInber := inberSlugFor(ma); pointsAtInber {
		ia, hasInber = inberCfg.Agents[slug]
	}
	return resolveAgentIdentity(ma, ia, hasInber)
}

// TestResolveAgentIdentityDerivesDisplayNamesFromIdsWithoutSplittingRunes is
// the regression test for `strings.ToUpper(ma.ID[:1]) + ma.ID[1:]`.
//
// ascii_control is a known-negative: the old spelling handled it correctly, so
// it must stay green when the fix is reverted. If every case reddens under the
// sabotage, the suite is not distinguishing the fix from the fixture.
func TestResolveAgentIdentityDerivesDisplayNamesFromIdsWithoutSplittingRunes(t *testing.T) {
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
				"agents": []map[string]any{{"id": c.id}},
			})
			if err != nil {
				t.Fatalf("building fixture JSON: %v", err)
			}
			mapping := parseMapping(t, string(raw))

			got := identityFor(mapping.Agents[0], InberConfig{}).DisplayName

			if got != c.want {
				t.Errorf("display name for id %q = %q, want %q", c.id, got, c.want)
			}
			// Separate from the equality check above: a mis-capitalised name and
			// a name that is no longer valid UTF-8 are different defects, and the
			// byte cut produced the second.
			if !utf8.ValidString(got) {
				t.Errorf("display name for id %q = % x, which is not valid UTF-8", c.id, got)
			}
		})
	}
}

// TestResolveAgentIdentityOnAnEmptyAgentIdDoesNotPanic pins the other half of
// the old spelling: ""[:1] is out of range. `{"id": ""}` is legal in
// mapping.json and json.Unmarshal accepts it without comment.
func TestResolveAgentIdentityOnAnEmptyAgentIdDoesNotPanic(t *testing.T) {
	mapping := parseMapping(t, `{"agents": [{"id": "", "project": "orphan"}]}`)

	got := identityFor(mapping.Agents[0], InberConfig{})

	if got.DisplayName != "" {
		t.Errorf("display name for the empty id = %q, want %q", got.DisplayName, "")
	}
	if got.Projects != "orphan" {
		t.Errorf("projects = %q, want %q — the rest of the entry must survive an empty id",
			got.Projects, "orphan")
	}
}

// TestResolveAgentIdentityWithoutAnInberEntryKeepsTheMappingValues pins the
// no-override path: the id fallback stands and role stays empty.
func TestResolveAgentIdentityWithoutAnInberEntryKeepsTheMappingValues(t *testing.T) {
	mapping := parseMapping(t, `{"agents": [{"id": "claxon", "project": "dash"}]}`)
	inberCfg := parseInberConfig(t, `{"agents": {"claxon": {"name": "Wrong", "role": "wrong"}}}`)

	// The mapping entry has no "inber" field, so the agents.json entry that
	// happens to share its id must NOT be picked up.
	got := identityFor(mapping.Agents[0], inberCfg)

	if got.DisplayName != "Claxon" {
		t.Errorf("display name = %q, want the id fallback %q", got.DisplayName, "Claxon")
	}
	if got.Role != "" {
		t.Errorf("role = %q, want empty — a mapping entry with no inber field has no role",
			got.Role)
	}
	if got.Projects != "dash" {
		t.Errorf("projects = %q, want the mapping value %q", got.Projects, "dash")
	}
}

// TestResolveAgentIdentityWithNoInberFieldDoesNotMatchAnEmptyKeyedEntry pins
// inberSlugFor's reason to exist.
//
// The lookup used to run with a zero slug for every non-inber agent. An
// agents.json carrying an entry keyed by the empty string would then have been
// applied to all of them at once.
func TestResolveAgentIdentityWithNoInberFieldDoesNotMatchAnEmptyKeyedEntry(t *testing.T) {
	mapping := parseMapping(t, `{"agents": [{"id": "claxon"}]}`)
	inberCfg := parseInberConfig(t, `{"agents": {"": {"name": "Empty Key", "role": "leaked"}}}`)

	got := identityFor(mapping.Agents[0], inberCfg)

	if got.DisplayName != "Claxon" {
		t.Errorf("display name = %q, want %q — an empty-keyed inber entry leaked into a non-inber agent",
			got.DisplayName, "Claxon")
	}
	if got.Role != "" {
		t.Errorf("role = %q, want empty — the empty-keyed inber entry was applied to an "+
			"agent that has no inber field", got.Role)
	}
}

// TestResolveAgentIdentityLetsAnInberEntryOverrideAllThreeFields pins the
// override itself.
func TestResolveAgentIdentityLetsAnInberEntryOverrideAllThreeFields(t *testing.T) {
	mapping := parseMapping(t, `{"agents": [{"id": "claxon", "inber": "claxon", "project": "dash"}]}`)
	inberCfg := parseInberConfig(t,
		`{"agents": {"claxon": {"name": "Claxon the Loud", "role": "herald", "projects": ["inber", "bus"]}}}`)

	got := identityFor(mapping.Agents[0], inberCfg)

	if got.DisplayName != "Claxon the Loud" {
		t.Errorf("display name = %q, want %q", got.DisplayName, "Claxon the Loud")
	}
	if got.Role != "herald" {
		t.Errorf("role = %q, want %q", got.Role, "herald")
	}
	if got.Projects != "inber,bus" {
		t.Errorf("projects = %q, want the joined list %q", got.Projects, "inber,bus")
	}
}

// TestResolveAgentIdentityPrefersTheProjectsListOverTheSingularProject pins the
// three-way precedence for projects, which no other case here distinguishes:
// the plural list wins, then the singular, then the mapping value.
func TestResolveAgentIdentityPrefersTheProjectsListOverTheSingularProject(t *testing.T) {
	cases := []struct {
		name     string
		inberRaw string
		want     string
	}{
		{
			"plural_list_wins",
			`{"agents": {"claxon": {"projects": ["inber"], "project": "singular"}}}`,
			"inber",
		},
		{
			"singular_used_when_list_is_empty",
			`{"agents": {"claxon": {"projects": [], "project": "singular"}}}`,
			"singular",
		},
		{
			"mapping_value_survives_when_inber_declares_neither",
			`{"agents": {"claxon": {"name": "Claxon"}}}`,
			"dash",
		},
	}

	mapping := parseMapping(t, `{"agents": [{"id": "claxon", "inber": "claxon", "project": "dash"}]}`)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := identityFor(mapping.Agents[0], parseInberConfig(t, c.inberRaw))
			if got.Projects != c.want {
				t.Errorf("projects = %q, want %q", got.Projects, c.want)
			}
		})
	}
}

// TestResolveAgentIdentityLetsAnEmptyInberNameBlankTheDisplayName records what
// seed does today. It is not an endorsement.
//
// The override assigns ia.Name unconditionally, so an inber entry with no name
// throws away the display name already derived from the id and stores "".
// cmd/migrate-inber guards the identical override with a != "" check and keeps
// the fallback, so the two tools disagree about the same field.
//
// Pinned rather than fixed because which one is right is a question about
// intent, not a defect with one obvious repair, and changing it here would
// silently rewrite display names on the next seed run. Filed separately.
func TestResolveAgentIdentityLetsAnEmptyInberNameBlankTheDisplayName(t *testing.T) {
	mapping := parseMapping(t, `{"agents": [{"id": "claxon", "inber": "claxon"}]}`)
	inberCfg := parseInberConfig(t, `{"agents": {"claxon": {"role": "herald"}}}`)

	got := identityFor(mapping.Agents[0], inberCfg)

	if got.DisplayName != "" {
		t.Errorf("display name = %q, want %q — if this now falls back to the id, seed's "+
			"behavior changed and the divergence card should be closed, not this test edited",
			got.DisplayName, "")
	}
}

// TestInberSlugForDistinguishesAnAbsentFieldFromAnEmptyOne — `"inber": ""` is a
// declared pointer at an empty slug and a missing "inber" is no pointer at all.
// Both produce the same slug, so only the boolean tells them apart.
func TestInberSlugForDistinguishesAnAbsentFieldFromAnEmptyOne(t *testing.T) {
	mapping := parseMapping(t, `{"agents": [{"id": "absent"}, {"id": "empty", "inber": ""}]}`)

	if _, points := inberSlugFor(mapping.Agents[0]); points {
		t.Error("a mapping entry with no inber field reports that it points at one")
	}
	slug, points := inberSlugFor(mapping.Agents[1])
	if !points {
		t.Error(`a mapping entry with "inber": "" reports that it points at nothing`)
	}
	if slug != "" {
		t.Errorf("slug = %q, want the empty string it declares", slug)
	}
}
