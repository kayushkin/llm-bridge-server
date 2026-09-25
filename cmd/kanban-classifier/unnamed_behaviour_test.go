package main

// Tests for behaviour that no test in this package named at all.
//
// A sabotage cross-table asks "which tests does no case redden". It cannot ask
// "which behaviour does no test name": a function absent from every _test.go is
// invisible to it, because there is no test there to be silent. Censusing this
// scorer's target against the package's tests, then emptying each candidate and
// re-running, found two functions whose whole body could be replaced by a
// zero-value return with the package still reporting ok.
//
// Both are executed by the suite — through callers that ARE tested — so
// coverage alone would have called them covered.
//
// Ported 2026-09-25 from 07db1186. Main renamed turn_count to messages_shown
// (8ca2373) and replaced the tool schema with reportSchema(), which the oneshot
// route sends as a bare JSON schema, so there is no tool name to check.

import (
	"strings"
	"testing"
)

// buildMetaSection was executed by every buildDigest test and asserted by none.
// Emptying it left the package green, and its three sibling section builders
// are each pinned by a test of their own — the shape that means someone stopped
// one short rather than never started.
//
// The section is how the model tells one session from another. Without
// bridge_id it cannot address a card back to the session it came from.
func TestBuildMetaSection_CarriesTheSessionIdentity(t *testing.T) {
	sess := session{
		SessionID:   "bridge-42",
		DisplayName: "nightly worker",
		Harness:     "claude_code",
		Purpose:     "scheduler",
		State:       "running",
		CreatedAt:   "2026-08-10T01:00:00Z",
		UpdatedAt:   "2026-08-10T03:30:00Z",
	}
	got := buildMetaSection(sess, 17)

	for _, want := range []string{
		"[SESSION META]",
		"bridge_id: bridge-42",
		"display_name: nightly worker",
		"harness: claude_code",
		"source: scheduler",
		"state: running",
		"created: 2026-08-10T01:00:00Z",
		"last_message: 2026-08-10T03:30:00Z",
		"messages_shown: 17",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("meta section is missing %q\ngot:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("meta section must end with a blank line so the next section starts clean, got %q", got)
	}
}

// reportSchema is the entire contract handed to the model: the fields it must
// fill, and the enum of states it may report.
// Nothing named it. Returning nil from it left the package green, so a renamed
// property or a dropped `required` would have shipped in silence — and the
// parser on the other side is pinned by hand-built fixtures that would keep
// passing, because they are written against the shape the schema no longer asks
// for.
func TestReportSchema_MatchesWhatTheParserReads(t *testing.T) {
	schema := reportSchema()

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties is %T, want map[string]any", schema["properties"])
	}
	for _, name := range []string{"trackable", "session_summary", "existing_cards", "new_tasks"} {
		if _, present := properties[name]; !present {
			t.Errorf("schema does not ask the model for %q", name)
		}
	}
	if required, _ := schema["required"].([]string); len(required) == 0 || required[0] != "trackable" {
		t.Errorf("schema.required = %v, want trackable — an untrackable session is the one answer the model must always give", schema["required"])
	}

	// The two array members carry the fields apply() indexes by. A card entry
	// without card_id cannot be matched back to a card.
	assertItemFields(t, properties, "existing_cards", []string{"card_id", "state", "summary", "blocker"}, []string{"card_id", "state", "summary"})
	assertItemFields(t, properties, "new_tasks", []string{"title", "state", "summary", "blocker"}, []string{"title", "state", "summary"})

	// The state enum is a closed vocabulary — the curator and the dispatcher
	// both branch on these exact strings.
	for _, arrayName := range []string{"existing_cards", "new_tasks"} {
		states := itemProperty(t, properties, arrayName, "state")
		enum, _ := states["enum"].([]string)
		if strings.Join(enum, ",") != "in_progress,blocked,done,abandoned" {
			t.Errorf("%s.state enum = %v, want in_progress,blocked,done,abandoned", arrayName, enum)
		}
	}
}

func itemsOf(t *testing.T, properties map[string]any, arrayName string) map[string]any {
	t.Helper()
	array, ok := properties[arrayName].(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want map[string]any", arrayName, properties[arrayName])
	}
	items, ok := array["items"].(map[string]any)
	if !ok {
		t.Fatalf("%s.items is %T, want map[string]any", arrayName, array["items"])
	}
	return items
}

func itemProperty(t *testing.T, properties map[string]any, arrayName, field string) map[string]any {
	t.Helper()
	itemProperties, ok := itemsOf(t, properties, arrayName)["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s.items.properties missing", arrayName)
	}
	value, ok := itemProperties[field].(map[string]any)
	if !ok {
		t.Fatalf("%s.items.properties.%s is %T, want map[string]any", arrayName, field, itemProperties[field])
	}
	return value
}

func assertItemFields(t *testing.T, properties map[string]any, arrayName string, want, wantRequired []string) {
	t.Helper()
	items := itemsOf(t, properties, arrayName)
	itemProperties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s.items.properties is %T, want map[string]any", arrayName, items["properties"])
	}
	for _, field := range want {
		if _, present := itemProperties[field]; !present {
			t.Errorf("%s entries do not offer %q", arrayName, field)
		}
	}
	required, _ := items["required"].([]string)
	if strings.Join(required, ",") != strings.Join(wantRequired, ",") {
		t.Errorf("%s.items.required = %v, want %v", arrayName, required, wantRequired)
	}
}

// The hundred-and-eighth pass measured that deleting the `(none)` placeholder on
// line 754 left the whole package green, and deliberately left this test undone
// because writing it mid-run would have moved the cross-table's test population.
// This is that test.
//
// The placeholder is what lets the model tell "this session had none of these"
// from "this section is missing from the digest". The missing-section reading is
// the one that made a live session mint six cards for one task in ninety
// minutes.
func TestBuildUserPromptsSection_NoPrompts(t *testing.T) {
	got := buildUserPromptsSection(nil, 4000)
	if !strings.Contains(got, "(none)") {
		t.Errorf("an empty prompt list must render the (none) placeholder, not a bare header\ngot:\n%q", got)
	}
}

// The same claim for the assistant-turn section, whose placeholder was equally
// unnamed. Kept separate so a failure names which section lost it.
//
// Its wording is NOT "(none)" — it is "(no assistant text content)", because an
// assistant turn that is all tool calls is a turn with nothing to quote rather
// than an absent turn. Asserting "(none)" here fails, which is how this test
// learned the difference; the property both sections share is that the section
// says something rather than ending at its header.
func TestBuildAssistantTurnsSection_NoTurns(t *testing.T) {
	got := buildAssistantTurnsSection(nil, 4000)
	if !strings.Contains(got, "(no assistant text content)") {
		t.Errorf("an empty turn list must say so, not end at the header\ngot:\n%q", got)
	}
}
