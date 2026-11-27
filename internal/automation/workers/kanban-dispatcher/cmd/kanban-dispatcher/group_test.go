package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// These tests cover the context group key: the value that decides which
// sub-cards may share one bridge session. An ungrouped card used to key on the
// empty string, so every ungrouped child of one parent formed a single group
// and reusableSiblingSession handed one of them another's live session.
//
// A test asserting only "two ungrouped cards land in different groups" would
// also pass against a build keying them on their titles, so the key tests
// assert the substituted key itself, and the tick tests assert the consequence
// through a whole tick. Ported from 2eed580 (fix/an-ungrouped-backlog-child-is-
// its-own-group), which reconciled two earlier authorings of this fix.

// groupOf is the group backlogGroups would build around one card.
func groupOf(parentID string, card issuesclient.CardView) backlogGroup {
	key, substituted := contextGroupKey(card)
	return backlogGroup{parentID: parentID, key: key, keyWasSubstituted: substituted, cards: []issuesclient.CardView{card}}
}

// ============================ the key itself ============================

func TestAGroupedCardKeysOnItsGroupTag(t *testing.T) {
	if key, substituted := contextGroupKey(testCard("card-a", "group:api")); key != "tag:api" || substituted {
		t.Errorf("contextGroupKey = %q/%v, want tag:api/false", key, substituted)
	}
}

func TestAnUngroupedCardKeysOnItsOwnCardID(t *testing.T) {
	if key, substituted := contextGroupKey(testCard("card-a", "cat:x")); key != "card:card-a" || !substituted {
		t.Errorf("contextGroupKey = %q/%v, want card:card-a/true", key, substituted)
	}
}

// A bare "group:" tag names no group. Trimming the prefix yields "", the exact
// value this key exists to keep out.
func TestABareGroupTagIsNoGroup(t *testing.T) {
	if key, substituted := contextGroupKey(testCard("card-a", "group:")); key != "card:card-a" || !substituted {
		t.Errorf("contextGroupKey = %q/%v, want card:card-a/true", key, substituted)
	}
}

// Skipping a blank group: tag must not stop the scan: a real tag can follow,
// and missing it would put a grouped card in a group of one.
func TestABlankGroupTagDoesNotHideARealOneAfterIt(t *testing.T) {
	if key, substituted := contextGroupKey(testCard("card-a", "group:", "group:api")); key != "tag:api" || substituted {
		t.Errorf("contextGroupKey = %q/%v, want tag:api/false", key, substituted)
	}
}

// Without namespaces a card tagged "group:card-a" would take the key the card
// whose id is "card-a" gets by substitution, and the two would share a session.
func TestATagCannotForgeTheKeyOfAnUngroupedCard(t *testing.T) {
	forger, _ := contextGroupKey(testCard("card-b", "group:card-a"))
	ungrouped, _ := contextGroupKey(testCard("card-a"))
	if forger == ungrouped {
		t.Errorf("card-b's tag group:card-a produced %q, the same key as ungrouped card-a", forger)
	}
}

// ============================ grouping ============================

func backlogGroupsOf(t *testing.T, backlog ...issuesclient.CardView) []backlogGroup {
	t.Helper()
	kanban := scopedBoard(t, map[string][]issuesclient.CardView{columnBacklog: backlog})
	server := httptest.NewServer(kanban)
	defer server.Close()
	return backlogGroups(issuesclient.New(server.URL), &kanban.view, "parent", degradations.New("dispatcher"))
}

func TestEachUngroupedChildIsAGroupOfOne(t *testing.T) {
	groups := backlogGroupsOf(t, testCard("card-a"), testCard("card-b"), testCard("card-c", "group:x"), testCard("card-d", "group:x"))
	var got []string
	for _, g := range groups {
		var ids []string
		for _, c := range g.cards {
			ids = append(ids, c.Placement.CardID)
		}
		got = append(got, strings.Join(ids, "+"))
	}
	if strings.Join(got, ",") != "card-a,card-b,card-c+card-d" {
		t.Errorf("groups = %v, want card-a and card-b alone and card-c+card-d together", got)
	}
}

// Group order follows the board, not the key: keys built from card ids would
// otherwise dispatch ungrouped sub-tasks in id order. The ids here sort the
// opposite way to their positions.
func TestGroupsAreOrderedByBoardPositionNotByKey(t *testing.T) {
	first, second, third := testCard("card-z"), testCard("card-m", "group:a"), testCard("card-b")
	first.Placement.Position, second.Placement.Position, third.Placement.Position = 1, 2, 3
	groups := backlogGroupsOf(t, third, first, second)
	var got []string
	for _, g := range groups {
		got = append(got, g.cards[0].Placement.CardID)
	}
	if strings.Join(got, ",") != "card-z,card-m,card-b" {
		t.Errorf("group order = %v, want board order card-z,card-m,card-b", got)
	}
}

// ============================ siblings and reuse ============================

func TestAnUngroupedCardHasNoSiblingButItself(t *testing.T) {
	kanban := scopedBoard(t, map[string][]issuesclient.CardView{
		columnBacklog: {testCard("card-next")},
		columnDone:    {testCard("card-finished"), testCard("card-grouped", "group:a")},
	})
	server := httptest.NewServer(kanban)
	defer server.Close()
	got := groupSiblings(issuesclient.New(server.URL), &kanban.view, groupOf("parent", testCard("card-next")), degradations.New("dispatcher"))
	if len(got) != 1 || got[0].Placement.CardID != "card-next" {
		t.Errorf("siblings = %v, want card-next alone", got)
	}
}

// The defect end to end. card-finished is ungrouped with an idle session
// updated a minute ago — the shape that IS revived inside a group (see
// TestAFreshIdleSiblingIsStillRevived). Two ungrouped cards wait in Backlog.
// Before the fix all three keyed on "", so one tick dispatched only one of the
// two, straight into card-finished's session.
func TestUngroupedCardsAreDispatchedTogetherIntoFreshSessions(t *testing.T) {
	kanban := scopedBoard(t, map[string][]issuesclient.CardView{
		columnBacklog: {testCard("card-a", "cat:product"), testCard("card-b", "cat:product")},
		columnDone:    {testCard("card-finished", "cat:product")},
	})
	kanban.effective["card-a"], kanban.effective["card-b"] = productCardDefaults(), productCardDefaults()
	kanban.status["card-finished"] = "done"
	kanban.links = map[string][]issuesclient.CardLink{
		"card-finished": {{CardID: "card-finished", EntityType: "session", EntityRef: "sess-sibling"}},
	}
	body := `{"state":"idle","updated_at":"` + time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) + `","instance_id":"inst-codex-local","agent_id":"northwind-builder","bundle_id":"6"}`
	err, published := tick(t, kanban, map[string]string{"sess-sibling": body})
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d, want both ungrouped cards in one tick: %+v", len(published), published)
	}
	for _, in := range published {
		if in.BusSessionID == "sess-sibling" || in.Create == nil {
			t.Errorf("an ungrouped card must get a fresh session of its own, got %q create=%v", in.BusSessionID, in.Create != nil)
		}
		if !strings.Contains(in.BusSessionID, "-ungrouped-card-") {
			t.Errorf("session id %q does not say its card was ungrouped", in.BusSessionID)
		}
	}
	if published[0].BusSessionID == published[1].BusSessionID {
		t.Errorf("both ungrouped cards were published into %q", published[0].BusSessionID)
	}
}

// ============================ what the worker is told ============================

func TestAnUngroupedWorkerIsToldItsSessionIsItsOwn(t *testing.T) {
	card := testCard("card-a")
	prompt := buildPrompt(scopedParent{cardID: "goal-1"}, card, groupOf("goal-1", card), "", testIssuesURL)
	if !strings.Contains(prompt, "- Group: none — this sub-task was not grouped with any sibling") {
		t.Errorf("the prompt must say the card has no group rather than print a substituted key:\n%s", prompt)
	}
	if strings.Contains(prompt, "card:card-a") || strings.Contains(prompt, "ungrouped:") {
		t.Errorf("the prompt leaks the substituted key:\n%s", prompt)
	}

	grouped := testCard("card-b", "group:api")
	if prompt := buildPrompt(scopedParent{cardID: "goal-1"}, grouped, groupOf("goal-1", grouped), "", testIssuesURL); !strings.Contains(prompt, "- Group: api\n") {
		t.Errorf("a grouped card's prompt must name its group without the namespace:\n%s", prompt)
	}
}
