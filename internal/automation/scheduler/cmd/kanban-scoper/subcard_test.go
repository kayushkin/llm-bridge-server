package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// These are the first tests for kanban-scoper. They cover the blank-group_key
// path, which was a fail-silent in three separate places: the card body
// rendered an empty pair of backticks, the plan summary rendered "Title[]", and
// the tag loop dropped the key without a word.
//
// The rule they are guarding, and the reason it is worth guarding: a blank
// group_key must stay VISIBLE and must never become an identity. It is fine for
// such a sub-task to be filed with no group — the dispatcher handles that
// correctly. It is not fine for it to look like a group whose name is "".

func testParent() parentCard {
	return parentCard{
		cardID: "card-123",
		item:   map[string]any{"title": "Ship the widget"},
	}
}

func TestBuildSubcardDraft_GroupedSubtaskGetsGroupTagAndBacktickedBody(t *testing.T) {
	d := buildSubcardDraft(testParent(), Subtask{
		Title:    "Add the migration",
		Body:     "Write it.",
		GroupKey: "kanban-store-schema",
	}, false)

	if !hasTag(d.Tags, tagGroupPrefix+"kanban-store-schema") {
		t.Fatalf("grouped sub-task lost its group tag: %v", d.Tags)
	}
	if !strings.Contains(d.Body, "group: `kanban-store-schema`") {
		t.Fatalf("real key should render inside backticks, got body:\n%s", d.Body)
	}
	if len(d.Notes) != 0 {
		t.Fatalf("a properly grouped sub-task should produce no notes, got %v", d.Notes)
	}
}

func TestBuildSubcardDraft_BlankGroupKeyEmitsNoGroupTag(t *testing.T) {
	// The point is NOT that a blank key is tolerated — it is that it must not
	// become a tag. A "group:" tag built from "" would put every ungrouped
	// sub-task into one group, which is the consumer-side defect this repo
	// already fixed once in kanban-dispatcher.
	d := buildSubcardDraft(testParent(), Subtask{Title: "Do a thing"}, false)

	for _, tag := range d.Tags {
		if strings.HasPrefix(tag, tagGroupPrefix) {
			t.Fatalf("blank group_key must emit no group tag, got %q", tag)
		}
	}
}

func TestBuildSubcardDraft_BlankGroupKeyIsReportedNotSwallowed(t *testing.T) {
	d := buildSubcardDraft(testParent(), Subtask{Title: "Do a thing"}, false)

	if len(d.Notes) == 0 {
		t.Fatal("a dropped group_key must be reported; this is the fail-silent the change exists to close")
	}
	if !strings.Contains(strings.Join(d.Notes, " "), "group_key") {
		t.Fatalf("the note should name what was lost, got %v", d.Notes)
	}
}

func TestBuildSubcardDraft_BodyNeverRendersAnEmptyPairOfBackticks(t *testing.T) {
	// This is the original defect, asserted literally so it cannot come back by
	// some other route (a format-string edit, a different display helper).
	for _, key := range []string{"", "   ", "\t\n"} {
		d := buildSubcardDraft(testParent(), Subtask{Title: "Do a thing", GroupKey: key}, false)
		if strings.Contains(d.Body, "``") {
			t.Fatalf("group_key %q rendered an empty pair of backticks:\n%s", key, d.Body)
		}
		if !strings.Contains(d.Body, groupKeyUnsetLabel) {
			t.Fatalf("group_key %q should render the named substitution, got:\n%s", key, d.Body)
		}
	}
}

func TestBuildSubcardDraft_WhitespaceOnlyGroupKeyIsTreatedAsBlank(t *testing.T) {
	// Without the trim, "   " is non-empty and produces the tag "group:   ",
	// which is a real tag naming a group nobody can ever join on purpose.
	d := buildSubcardDraft(testParent(), Subtask{Title: "Do a thing", GroupKey: "   "}, false)

	for _, tag := range d.Tags {
		if strings.HasPrefix(tag, tagGroupPrefix) {
			t.Fatalf("whitespace-only key must not become a tag, got %q", tag)
		}
	}
	if len(d.Notes) == 0 {
		t.Fatal("whitespace-only key is a lost grouping and must be reported")
	}
}

func TestBuildSubcardDraft_GroupKeyIsTrimmedBeforeBecomingATag(t *testing.T) {
	d := buildSubcardDraft(testParent(), Subtask{Title: "Do a thing", GroupKey: "  auth-store-tests\n"}, false)

	if !hasTag(d.Tags, tagGroupPrefix+"auth-store-tests") {
		t.Fatalf("group tag should carry the trimmed key, got %v", d.Tags)
	}
}

func TestBuildSubcardDraft_TitleIsTrimmedAndBlankTitleIsLeftForTheCaller(t *testing.T) {
	d := buildSubcardDraft(testParent(), Subtask{Title: "  Do a thing  "}, false)
	if d.Title != "Do a thing" {
		t.Fatalf("title should be trimmed, got %q", d.Title)
	}

	// A blank title comes back blank rather than as an error: scopeParent skips
	// it, because only the caller knows the sub-task's index for the log line.
	blank := buildSubcardDraft(testParent(), Subtask{Title: "   "}, false)
	if blank.Title != "" {
		t.Fatalf("whitespace-only title should reduce to empty, got %q", blank.Title)
	}
}

func TestBuildSubcardDraft_EmptyBodyGetsAPlaceholder(t *testing.T) {
	d := buildSubcardDraft(testParent(), Subtask{Title: "Do a thing", GroupKey: "g"}, false)
	if !strings.Contains(d.Body, "(no body)") {
		t.Fatalf("an empty body should be marked, got:\n%s", d.Body)
	}
}

func TestBuildSubcardDraft_RoleTagsOnlyWhenEnabled(t *testing.T) {
	st := Subtask{Title: "Do a thing", GroupKey: "g", Capability: "go-http", Role: "builder"}

	off := buildSubcardDraft(testParent(), st, false)
	if hasTag(off.Tags, "capability:go-http") || hasTag(off.Tags, "role:builder") {
		t.Fatalf("role tags must stay off by default, got %v", off.Tags)
	}

	on := buildSubcardDraft(testParent(), st, true)
	if !hasTag(on.Tags, "capability:go-http") || !hasTag(on.Tags, "role:builder") {
		t.Fatalf("role tags should be emitted when enabled, got %v", on.Tags)
	}
}

func TestBuildSubcardDraft_LinksBackToTheParentCard(t *testing.T) {
	d := buildSubcardDraft(testParent(), Subtask{Title: "Do a thing", GroupKey: "g"}, false)
	if !strings.Contains(d.Body, "kanban://card-123") {
		t.Fatalf("body should link to the parent card, got:\n%s", d.Body)
	}
	if !strings.Contains(d.Body, "Ship the widget") {
		t.Fatalf("body should name the parent, got:\n%s", d.Body)
	}
}

func TestDisplayGroupKeyIsNeverConfusableWithARealKey(t *testing.T) {
	// Pin the literal marker text once. Every other assertion in this file
	// compares against the groupKeyUnsetLabel constant, which means none of them
	// can detect the constant itself being changed to something useless — set it
	// to "" and `strings.Contains(body, "")` is vacuously true everywhere. This
	// is the one assertion that would go red for that.
	if groupKeyUnsetLabel != "(unset)" {
		t.Fatalf("the unset marker is load-bearing prose in card bodies and log lines; got %q", groupKeyUnsetLabel)
	}
	if strings.TrimSpace(groupKeyUnsetLabel) == "" {
		t.Fatal("an empty marker would make the substitution invisible, which is the defect this closed")
	}

	// The substitution is only safe because a reader can tell it apart from a
	// key the decomposer returned. A real key always arrives backticked; the
	// marker never does.
	if strings.Contains(displayGroupKey(""), "`") {
		t.Fatalf("the unset marker must not be backticked, got %q", displayGroupKey(""))
	}
	if !strings.HasPrefix(displayGroupKey("real"), "`") {
		t.Fatalf("a real key must be backticked, got %q", displayGroupKey("real"))
	}

	// And if the decomposer ever literally returns the marker text, it is a real
	// key and must render as one — otherwise the marker becomes an identity.
	asKey := displayGroupKey(groupKeyUnsetLabel)
	if asKey == displayGroupKey("") {
		t.Fatalf("a literal %q key must not render identically to a missing one", groupKeyUnsetLabel)
	}
}

func TestSummarizePlan_BlankKeyDoesNotRenderAsAGroupNamedEmptyString(t *testing.T) {
	plan := &Plan{Subtasks: []Subtask{
		{Title: "A", GroupKey: "grp"},
		{Title: "B"},
	}}
	got := summarizePlan(plan)

	if strings.Contains(got, "B[]") {
		t.Fatalf("blank key rendered as an empty group in the summary: %q", got)
	}
	if !strings.Contains(got, "B["+groupKeyUnsetLabel+"]") {
		t.Fatalf("blank key should render the named substitution, got %q", got)
	}
	if !strings.Contains(got, "A[`grp`]") {
		t.Fatalf("a real key should still render, got %q", got)
	}
}

func TestDecomposeSchema_GroupKeyRejectsTheEmptyString(t *testing.T) {
	// "required" alone does not reject "": an empty string satisfies
	// {"type":"string"}. minLength is what makes the stated requirement true.
	//
	// This asserts the schema we BUILD. It cannot tell you the Anthropic API
	// accepts it — a schema the provider rejects would break every tick, and
	// that is a live-call question. Verified by hand against
	// claude-haiku-4-5-20251001 on 2026-08-08, both roleTags settings: accepted,
	// 10 and 8 sub-tasks returned, zero blank group keys. Re-verify by hand if
	// this schema changes; do not read this test as covering it. That check was
	// made when the schema was a tool's input_schema on api.anthropic.com; it now
	// goes out bare through the oneshot bridge path, which has not been re-checked.
	for _, roleTags := range []bool{false, true} {
		schema := decomposeSchema(roleTags)
		props := schema["properties"].(map[string]any)["subtasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
		gk := props["group_key"].(map[string]any)
		if gk["minLength"] != 1 {
			t.Fatalf("roleTags=%v: group_key needs minLength 1, got %#v", roleTags, gk)
		}
		// The schema goes out over the wire as JSON; an unmarshalable value here
		// fails the tick rather than this test, which is the wrong place to find out.
		if _, err := json.Marshal(schema); err != nil {
			t.Fatalf("roleTags=%v: schema does not marshal: %v", roleTags, err)
		}
	}
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
