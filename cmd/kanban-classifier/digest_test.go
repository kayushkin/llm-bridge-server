package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
)

func card(id, title, body string) kanbanclient.EntityCardEntry {
	return kanbanclient.EntityCardEntry{
		CardID: id,
		Item: map[string]any{
			"title":  title,
			"status": "open",
			"body":   body,
		},
	}
}

// longSession mimics the real shape that broke the classifier: dozens of long
// user prompts, so the conversational bulk alone dwarfs the whole budget.
func longSession(prompts, promptLen int) []bridgeMessage {
	msgs := []bridgeMessage{}
	for i := 0; i < prompts; i++ {
		msgs = append(msgs,
			bridgeMessage{Role: "user", Content: fmt.Sprintf("prompt-%d ", i) + strings.Repeat("x", promptLen)},
			bridgeMessage{Role: "assistant", Content: fmt.Sprintf("reply-%d ", i) + strings.Repeat("y", promptLen)},
		)
	}
	return msgs
}

// The bug: on a long session the existing-cards section fell off the end of
// the digest, so the model was asked not to duplicate cards it could not see.
func TestBuildDigest_KeepsExistingCardsOnLongSession(t *testing.T) {
	msgs := longSession(40, 1400)
	cards := []kanbanclient.EntityCardEntry{
		card("card-1", "Build orchestrator task summary split view", strings.Repeat("b", 900)),
		card("card-2", "Fix orchestrator chat opening from sidebar", strings.Repeat("b", 900)),
		card("card-3", "Design context injection strategy", strings.Repeat("b", 900)),
	}

	digest := buildDigest(session{SessionID: "br_1"}, msgs, cards, digestCharBudget)

	if len(digest) > digestCharBudget {
		t.Fatalf("digest %d chars exceeds budget %d", len(digest), digestCharBudget)
	}
	if !strings.Contains(digest, "[EXISTING CARDS") {
		t.Fatal("existing-cards section missing from digest")
	}
	for _, c := range []string{"card-1", "card-2", "card-3"} {
		if !strings.Contains(digest, c) {
			t.Errorf("card %s missing from digest — the model cannot dedupe against a card it is not shown", c)
		}
	}
	if !strings.Contains(digest, "[USER PROMPTS") || !strings.Contains(digest, "[ASSISTANT TURNS") || !strings.Contains(digest, "[TOOL ACTIVITY") {
		t.Error("a conversational section was dropped entirely; sections must shrink, not vanish")
	}
}

// The newest prompt is the current intent. Fitting must drop the oldest, and
// say how many it dropped.
func TestBuildDigest_KeepsNewestPromptsAndAnnouncesDrops(t *testing.T) {
	msgs := longSession(40, 1400)
	digest := buildDigest(session{SessionID: "br_1"}, msgs, nil, digestCharBudget)

	if !strings.Contains(digest, "prompt-39") {
		t.Error("newest user prompt missing — the digest kept the oldest instead")
	}
	if !strings.Contains(digest, "earlier prompt(s) omitted") {
		t.Error("dropped prompts were not announced")
	}
	if !strings.Contains(digest, "reply-39") {
		t.Error("newest assistant turn missing")
	}
}

func TestBuildDigest_ShortSessionIsWhole(t *testing.T) {
	msgs := []bridgeMessage{
		{Role: "user", Content: "fix the login redirect"},
		{Role: "assistant", Content: "done, pushed as abc123"},
	}
	digest := buildDigest(session{SessionID: "br_1"}, msgs, []kanbanclient.EntityCardEntry{card("card-1", "Fix login redirect", "in progress")}, digestCharBudget)

	for _, want := range []string{"fix the login redirect", "done, pushed as abc123", "card-1", "Fix login redirect"} {
		if !strings.Contains(digest, want) {
			t.Errorf("%q missing from a digest that fits the budget whole", want)
		}
	}
	if strings.Contains(digest, "omitted") {
		t.Error("nothing should be omitted from a session this small")
	}
}

// Card identity survives even when the cards alone would blow the reserve:
// bodies are shed first, titles last.
func TestBuildExistingCardsSection_ShedsBodiesBeforeCards(t *testing.T) {
	cards := []kanbanclient.EntityCardEntry{}
	for i := 0; i < 20; i++ {
		cards = append(cards, card(fmt.Sprintf("card-%d", i), fmt.Sprintf("Task number %d", i), strings.Repeat("b", 400)))
	}
	section := buildExistingCardsSection(cards, budgetShare(digestCharBudget, existingCardsBudgetShare))

	for i := 0; i < 20; i++ {
		if !strings.Contains(section, fmt.Sprintf("card-%d", i)) {
			t.Fatalf("card-%d dropped while bodies were still being carried", i)
		}
	}
	if strings.Contains(section, strings.Repeat("b", 400)) {
		t.Error("full bodies kept even though titles had to compete for room")
	}
}

func TestBuildExistingCardsSection_NoCards(t *testing.T) {
	if got := buildExistingCardsSection(nil, 1000); !strings.Contains(got, "(none)") {
		t.Errorf("empty card list should render (none), got %q", got)
	}
}

func TestFitKeepingNewest_KeepsOneEntryEvenWhenOversized(t *testing.T) {
	got := fitKeepingNewest([]string{"aaaa", strings.Repeat("z", 500)}, 10, "thing(s) omitted")
	if !strings.Contains(got, strings.Repeat("z", 500)) {
		t.Error("the newest entry must survive even when it alone exceeds the budget")
	}
	if !strings.Contains(got, "1 thing(s) omitted") {
		t.Errorf("drop not announced: %q", got[:40])
	}
}

func TestNormalizeTitle(t *testing.T) {
	same := []([2]string){
		{"Build orchestrator task summary split view", "build  orchestrator, task summary — split view!"},
		{"Fix Orchestrator row chat-open action", "fix orchestrator row chat open action"},
	}
	for _, pair := range same {
		if normalizeTitle(pair[0]) != normalizeTitle(pair[1]) {
			t.Errorf("%q and %q should normalize equal", pair[0], pair[1])
		}
	}
	if normalizeTitle("Build split view") == normalizeTitle("Build split-view dashboard") {
		t.Error("distinct titles must not collapse — a fuzzy match would suppress real tasks")
	}
}
