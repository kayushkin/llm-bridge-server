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

// TestFitKeepingNewest_NeverExceedsTheBudgetItWasHanded pins the marker
// accounting directly, because the call-site assertion in
// TestBuildDigestOverBudgetIsStillValidUTF8 detects it only by luck of
// arithmetic: on that fixture it reddens at four of eight pads, and at the
// other four the section had enough slack to absorb the marker. A detector
// that depends on the fixture's spare room is one resize away from silence.
//
// So sweep every budget instead of picking one. The interesting range is
// narrow and the marker's length changes with the count it announces, so a
// hand-chosen budget can sit either side of a digit boundary and prove
// nothing.
func TestFitKeepingNewest_NeverExceedsTheBudgetItWasHanded(t *testing.T) {
	entries := make([]string, 12)
	for i := range entries {
		entries[i] = fmt.Sprintf("%02d %s\n", i, strings.Repeat("x", 40))
	}
	newest := entries[len(entries)-1]

	// The documented exception, pinned rather than stepped around: handed less
	// than the newest entry and the marker announcing everything else need,
	// fitKeepingNewest emits both anyway and goes over. Rendering the section
	// empty, or keeping the entry while hiding the drop, would each be worse —
	// the digest's rule is that every drop is announced in the text.
	//
	// 26 is that marker at its longest here, "(… 11 thing(s) omitted)\n",
	// written as a literal so a change to the marker's wording fails this test
	// instead of silently moving the boundary it is supposed to fix.
	const longestMarker = 26
	if got := fitKeepingNewest(entries, len(newest), "thing(s) omitted"); len(got) != len(newest)+longestMarker {
		t.Fatalf("at a budget of exactly the newest entry, want it plus the %d-character marker, got %d characters: %q",
			longestMarker, len(got), got)
	}

	markerSeen, wholeSeen := 0, 0
	for budget := len(newest) + longestMarker; budget <= totalLen(entries)+40; budget++ {
		got := fitKeepingNewest(entries, budget, "thing(s) omitted")

		if len(got) > budget {
			t.Fatalf("budget=%d: returned %d characters, over by %d: %q",
				budget, len(got), len(got)-budget, got)
		}
		if !strings.HasSuffix(got, newest) {
			t.Fatalf("budget=%d: the newest entry was dropped", budget)
		}
		// The marker is only worth charging for if it tells the truth about
		// how many entries it stands in for.
		if kept := strings.Count(got, "\n") - strings.Count(got, "omitted)"); kept != len(entries) {
			if !strings.Contains(got, fmt.Sprintf("(… %d thing(s) omitted)", len(entries)-kept)) {
				t.Fatalf("budget=%d: kept %d of %d entries and announced none of the drop: %q",
					budget, kept, len(entries), got)
			}
			markerSeen++
		} else {
			wholeSeen++
		}
	}

	// Without both, the sweep above is satisfied by a helper that always drops
	// everything or never drops anything, and the marker is never charged at all.
	if markerSeen == 0 || wholeSeen == 0 {
		t.Fatalf("sweep covered %d budgets that dropped entries and %d that fit them whole; both must be non-zero",
			markerSeen, wholeSeen)
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
