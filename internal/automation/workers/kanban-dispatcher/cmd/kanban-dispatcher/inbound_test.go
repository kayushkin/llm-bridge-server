package main

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// Ported from 18ecd596, 652acbfd and f119a711 (test/the-mint-site-truncates-a-
// long-parent-id) onto main's buildBridgeInbound, which takes a cardSessionSpec
// in place of the old compiled-in harness and instance.
//
// Every value below is read by another program, so each is written as a
// literal: a test that compared against the code's own constant would follow
// the code wherever it drifted.

func TestAFreshSessionIsCreatedWithTheValuesTheBridgeFilesItUnder(t *testing.T) {
	spec := cardSessionSpec{Harness: msg.HarnessCodex, InstanceID: "inst-codex-local", AgentSlug: "northwind-builder", BundleID: "6", CardID: "card-a"}
	in, err := buildBridgeInbound("tcl-sess", "tcl a · Sep 25 22:00", "do the thing", true, spec, "default")
	if err != nil {
		t.Fatal(err)
	}

	if in.BusSessionID != "tcl-sess" || in.Send == nil || in.Send.Message != "do the thing" {
		t.Errorf("the message must carry the session id and the prompt, got session %q send %+v", in.BusSessionID, in.Send)
	}
	if in.Agent != "tcl-worker" || in.Orchestrator != "task-completion-loop" || in.Channel != "task-completion-loop" {
		t.Errorf("envelope labels = agent %q orchestrator %q channel %q", in.Agent, in.Orchestrator, in.Channel)
	}
	if in.Timestamp.Location() != time.UTC {
		t.Errorf("the envelope must be stamped in UTC, got %v", in.Timestamp.Location())
	}
	c := in.Create
	if c == nil {
		t.Fatal("a fresh session must carry a Create")
	}
	if c.Purpose != "dispatcher" {
		t.Errorf("purpose = %q, want dispatcher (llm-bridge's purpose registry)", c.Purpose)
	}
	if c.Origin != "scheduler" {
		t.Errorf("origin = %q, want scheduler", c.Origin)
	}
	if c.Type != msg.SessionTypeAutonomous {
		t.Errorf("type = %q, want %q", c.Type, msg.SessionTypeAutonomous)
	}
	if !c.AutoStart {
		t.Error("auto_start must be true, or the session is created and prompted and never runs")
	}
	if c.DisplayName != "tcl a · Sep 25 22:00" {
		t.Errorf("display name = %q", c.DisplayName)
	}
}

func TestARevivedSessionIsSentToAndNotCreatedAgain(t *testing.T) {
	in, err := buildBridgeInbound("sess-sibling", "x", "next task", false, cardSessionSpec{InstanceID: "inst-cc-local"}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if in.Create != nil {
		t.Errorf("a revived session exists already; asking the bridge to create it again is wrong, got %+v", in.Create)
	}
	if in.Send == nil || in.Send.Message != "next task" {
		t.Errorf("a revived session must still be sent the prompt, got %+v", in.Send)
	}
}

func TestThePromptCarriesTheSubTaskAndTellsTheWorkerToCloseItsOwnCard(t *testing.T) {
	parent := scopedParent{cardID: "goal-1", item: map[string]any{"title": "Ship it"}}
	child := testCard("child-7")
	child.Item["body"] = "Fix the flaky test in foo_test.go."

	prompt := buildPrompt(parent, child, groupOf("goal-1", child), "")
	if !strings.Contains(prompt, "Fix the flaky test in foo_test.go.") {
		t.Errorf("the prompt must carry the card's body — it is the task:\n%s", prompt)
	}
	if !strings.Contains(prompt, "PATCH http://localhost:8191/api/items/child-7 ") {
		t.Errorf("the worker must be told to close its own card, child-7:\n%s", prompt)
	}
	if strings.Contains(prompt, "/api/items/goal-1") {
		t.Errorf("the worker must never be told to close the parent goal:\n%s", prompt)
	}
	if !strings.Contains(prompt, "You are picking up a sub-task") {
		t.Errorf("a fresh session must be introduced to the workflow:\n%s", prompt)
	}

	revived := buildPrompt(parent, child, groupOf("goal-1", child), "sess-sibling")
	if !strings.Contains(revived, "you already have context from a sibling task") {
		t.Errorf("a revived session must be told its context comes from a sibling:\n%s", revived)
	}
}

func TestABodylessCardSaysSoInsteadOfLeavingABlank(t *testing.T) {
	child := testCard("child-7")
	prompt := buildPrompt(scopedParent{cardID: "goal-1"}, child, groupOf("goal-1", child), "")
	if !strings.Contains(prompt, "(no body — see title)") {
		t.Errorf("a card with no body must say so where the task goes:\n%s", prompt)
	}
}

func TestAnUntitledCardIsNamedByItsID(t *testing.T) {
	if got := (scopedParent{cardID: "goal-1", item: map[string]any{}}).title(); got != "goal-1" {
		t.Errorf("an untitled parent's title = %q, want its id", got)
	}
	untitled := issuesclient.CardView{Item: map[string]any{}}
	untitled.Placement.CardID = "child-7"
	if got := childTitle(untitled); got != "child-7" {
		t.Errorf("an untitled child's title = %q, want its id", got)
	}
	// Ported from ef5bee2a: an empty or non-string title falls back to the id
	// too, and a real one is used as written.
	for name, title := range map[string]any{"empty": "", "non-string": 42} {
		card := issuesclient.CardView{Item: map[string]any{"title": title}}
		card.Placement.CardID = "child-8"
		if got := childTitle(card); got != "child-8" {
			t.Errorf("a child with a %s title is called %q, want its id", name, got)
		}
	}
	titled := issuesclient.CardView{Item: map[string]any{"title": "Port the sidebar"}}
	titled.Placement.CardID = "child-9"
	if got := childTitle(titled); got != "Port the sidebar" {
		t.Errorf("a titled child is called %q, want its title", got)
	}
}

// ============================ the mint site ============================

// Fixtures use a 36-byte uuid: at exactly 8 bytes, shortID(id) and a raw
// id[:8] return the same string, which is how every mutation of this site
// once scored unnoticed.
const longParentID = "0f8e2a1c-4b5d-4e6f-8a9b-0c1d2e3f4a5b"

func TestAMintedSessionIDNamesItsParentAndGroup(t *testing.T) {
	id := mintSessionID(longParentID, groupOf(longParentID, testCard("child-7", "group:api work")))
	if !regexp.MustCompile(`^tcl-0f8e2a1c-api-work-\d+$`).MatchString(id) {
		t.Errorf("minted %q, want tcl-<first 8 of the parent>-<group, spaces as dashes>-<nanoseconds>", id)
	}
}

// An ungrouped card is a group of its own, and its session id says so rather
// than naming a shared "default" group every ungrouped card would read as.
func TestAnUngroupedCardMintsItsOwnIDAsTheGroup(t *testing.T) {
	id := mintSessionID(longParentID, groupOf(longParentID, testCard("9a8b7c6d-1111-2222-3333-444455556666")))
	if !regexp.MustCompile(`^tcl-0f8e2a1c-ungrouped-9a8b7c6d-\d+$`).MatchString(id) {
		t.Errorf("minted %q, want the group segment to read ungrouped-<first 8 of the card id>", id)
	}
}

// A display key can carry a colon or a space, and neither belongs in a session
// id; each run of them becomes one dash.
func TestASessionIDCarriesNoColonOrSpace(t *testing.T) {
	id := mintSessionID("p1", groupOf("p1", testCard("child-7", "group:api: v2")))
	if !regexp.MustCompile(`^tcl-p1-api-v2-\d+$`).MatchString(id) {
		t.Errorf("minted %q, want the colon and space in the group collapsed to one dash", id)
	}
}

func TestAShortParentIDIsMintedWhole(t *testing.T) {
	id := mintSessionID("p1", groupOf("p1", testCard("child-7", "group:a")))
	if !regexp.MustCompile(`^tcl-p1-a-\d+$`).MatchString(id) {
		t.Errorf("minted %q from a 2-byte parent id", id)
	}
}

func TestShortIDKeepsEightBytes(t *testing.T) {
	if got := shortID(longParentID); got != "0f8e2a1c" {
		t.Errorf("shortID(%q) = %q, want 0f8e2a1c", longParentID, got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("shortID(abc) = %q", got)
	}
	// Nine bytes is the one length that tells `len(id) <= 8` from `<= 9`: at
	// eight and below the guard and the cut return the same string.
	if got := shortID("123456789"); got != "12345678" {
		t.Errorf("shortID(123456789) = %q, want the first 8 bytes", got)
	}
	// Ported from e74e77b8, at the call site whose result leaves the process: a
	// four-byte rune across byte 8 is dropped whole, not split.
	if got := mintSessionID("abcde\U0001D11Exyz", groupOf("p1", testCard("child-7", "group:a"))); !strings.HasPrefix(got, "tcl-abcde-a-") {
		t.Errorf("mintSessionID = %+q, want the parent cut back to %q before the rune", got, "abcde")
	}
}

// ============================ reading tags ============================

func TestATagThatIsNotAStringDoesNotHideTheTagsAfterIt(t *testing.T) {
	got := stringSlice([]any{"cat:x", 7, "group:a"})
	if strings.Join(got, ",") != "cat:x,group:a" {
		t.Errorf("stringSlice = %v, want the two strings either side of the number", got)
	}
	card := testCard("child-7")
	card.Item["tags"] = []any{nil, "group:a"}
	if key, substituted := contextGroupKey(card); key != "tag:a" || substituted {
		t.Errorf("contextGroupKey = %q/%v, want tag:a/false — a card whose first tag is not a string must still find its group", key, substituted)
	}
}

func TestTheScopedTagMatchesWhole(t *testing.T) {
	tags := []issuesclient.EntityTag{{Tag: "scoped-later"}}
	if anyTagEquals(tags, "scoped") {
		t.Error("scoped-later is not the scoped tag")
	}
	if !anyTagEquals([]issuesclient.EntityTag{{Tag: "x"}, {Tag: "scoped"}}, "scoped") {
		t.Error("the scoped tag must be found wherever it sits")
	}
}

func TestTheGroupTagPrefixIsGroupColon(t *testing.T) {
	if key, _ := contextGroupKey(testCard("child-7", "cat:x", "group:api")); key != "tag:api" {
		t.Errorf("contextGroupKey = %q, want tag:api", key)
	}
}
