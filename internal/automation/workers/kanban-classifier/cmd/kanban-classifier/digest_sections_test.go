package main

import (
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// The digest is the whole of what this binary buys. Every tick it does not
// skip, it renders one of these and pays a model to read it — up to ten
// sessions a tick, ninety-six ticks a day.
//
// digest_test.go already pins the SHAPE of that spend: which sections survive
// the budget, in what order, and that nothing vanishes silently. What it does
// not pin is the CONTENT of the two sections a reader of the digest reasons
// from — [SESSION META], which says which session this is and whether it is
// still running, and [USER PROMPTS], which is the only verbatim record of what
// was asked. Both were executed by every existing test on the way to an
// assertion about length or section order, and observed by none of them:
// buildMetaSection could name the wrong session, report a completed session as
// running, and claim every session had said nothing, and the suite stayed
// green.
//
// The failure these guard against is the quiet one. A digest that is missing a
// field is not a digest that errors; it is a digest the model answers anyway,
// slightly wrong, at full price, ninety-six times a day.

func metaSection(t *testing.T, digest string) string {
	t.Helper()
	if !strings.HasPrefix(digest, "[SESSION META]\n") {
		t.Fatalf("digest does not open with the meta section:\n%s", digest[:min(200, len(digest))])
	}
	end := strings.Index(digest, "[EXISTING CARDS")
	if end < 0 {
		t.Fatalf("no [EXISTING CARDS] section to bound the meta section by:\n%s", digest)
	}
	return digest[:end]
}

func sampleSession() session {
	return session{
		SessionID:   "br_9f21",
		DisplayName: "nightly worker",
		Harness:     "claude-code",
		Purpose:     "autoworker",
		State:       "completed",
		CreatedAt:   "2026-08-10T09:00:00Z",
		UpdatedAt:   "2026-08-10T11:30:00Z",
	}
}

// The bridge id is the only thing tying the model's report back to a session.
// Every card the classifier writes is keyed on it, and the report the model
// returns names cards, not sessions — so a digest that omits it asks the model
// to reason about a session it cannot identify, and two sessions with the same
// display name become one.
func TestTheMetaSectionNamesTheSessionTheModelIsReadingAbout(t *testing.T) {
	digest := buildDigest(sampleSession(), nil, nil, digestCharBudget)

	if !strings.Contains(metaSection(t, digest), "bridge_id: br_9f21") {
		t.Fatalf("the meta section does not name the session:\n%s", metaSection(t, digest))
	}
}

// Bridge state and source are different questions asked one line apart, and
// the system prompt tells the model to weigh them differently — "Bridge state ≠
// task state". Rendering one where the other belongs is invisible in the
// output's shape: the section is the same length and every line is still
// present.
func TestTheMetaSectionKeepsTheSessionsStateAndItsSourceApart(t *testing.T) {
	sess := sampleSession()
	meta := metaSection(t, buildDigest(sess, nil, nil, digestCharBudget))

	for _, want := range []string{
		"state: " + sess.State,
		"source: " + sess.Purpose,
		"harness: " + sess.Harness,
		"display_name: " + sess.DisplayName,
		"created: " + sess.CreatedAt,
		"last_message: " + sess.UpdatedAt,
	} {
		if !strings.Contains(meta, want+"\n") {
			t.Errorf("meta section is missing %q:\n%s", want, meta)
		}
	}
}

// messages_shown is the number of messages the digest was built from. It was
// turn_count until 8ca2373 bounded the read to the newest turns, when it stopped
// being the session's total. Reported as zero, every session reads as one that
// has said nothing yet — which is exactly the shape the model is told to answer
// trackable=false to.
func TestTheMetaSectionCountsTheMessagesItWasGiven(t *testing.T) {
	msgs := []bridgeMessage{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
	}

	meta := metaSection(t, buildDigest(sampleSession(), msgs, nil, digestCharBudget))

	if !strings.Contains(meta, "messages_shown: 3\n") {
		t.Fatalf("messages_shown does not report the %d messages the digest was built from:\n%s", len(msgs), meta)
	}
}

// Every section ends with a blank line so the next header starts one. Without
// it the meta section's last field and the [EXISTING CARDS] header share a
// line, and the header stops being a header.
func TestASectionHeaderAlwaysStartsItsOwnLine(t *testing.T) {
	digest := buildDigest(sampleSession(), longSession(3, 40), []issuesclient.EntityCardEntry{
		card("card-1", "a card", "a body"),
	}, digestCharBudget)

	for _, header := range []string{"[EXISTING CARDS", "[USER PROMPTS", "[ASSISTANT TURNS", "[TOOL ACTIVITY"} {
		i := strings.Index(digest, header)
		if i < 0 {
			t.Errorf("section %s missing from the digest", header)
			continue
		}
		if i == 0 {
			continue
		}
		if !strings.HasPrefix(digest[i-2:], "\n\n") {
			t.Errorf("%s does not start its own line — the section before it ran into it:\n...%q...",
				header, digest[max(0, i-40):i+len(header)])
		}
	}
}

// The section is headed "verbatim", and the user's own words are the strongest
// evidence in the digest of what the session is for. Filling it with the
// assistant's replies instead gives the model its own prior output to classify
// — and an assistant that narrates what it is about to do reads exactly like a
// user asking for it.
func TestTheUserPromptsSectionCarriesTheUsersWordsAndNotTheAssistants(t *testing.T) {
	msgs := []bridgeMessage{
		{Role: "user", Content: "migrate the auth middleware"},
		{Role: "assistant", Content: "I will migrate the auth middleware"},
	}

	digest := buildDigest(sampleSession(), msgs, nil, digestCharBudget)
	prompts := sectionOf(t, digest, "[USER PROMPTS")

	if !strings.Contains(prompts, "migrate the auth middleware") {
		t.Fatalf("the user's prompt is not in the prompts section:\n%s", prompts)
	}
	if strings.Contains(prompts, "I will migrate") {
		t.Fatalf("an assistant turn was rendered as a user prompt:\n%s", prompts)
	}
}

// A user message with nothing in it is a turn the bridge recorded, not
// something that was asked. Numbering it spends budget on a blank line and
// tells the model the user spoke when they did not — and on a long session
// those blanks push real prompts out of the section.
func TestAnEmptyUserMessageIsNotNumberedAsAPrompt(t *testing.T) {
	msgs := []bridgeMessage{
		{Role: "user", Content: "the only real ask"},
		{Role: "user", Content: "   \n\t "},
		{Role: "user", Content: ""},
	}

	prompts := sectionOf(t, buildDigest(sampleSession(), msgs, nil, digestCharBudget), "[USER PROMPTS")

	if !strings.Contains(prompts, "1. the only real ask") {
		t.Fatalf("the real prompt is not numbered first:\n%s", prompts)
	}
	if strings.Contains(prompts, "2.") {
		t.Fatalf("a blank user message was numbered as a prompt:\n%s", prompts)
	}
}

// fitKeepingNewest deliberately keeps one entry even when that entry alone
// overflows the budget — a section that renders empty is how the original
// truncation bug hid. That rule makes the per-prompt bound the only thing
// standing between one pasted log and a digest of nothing else, because the
// oversized entry is the one entry that is always kept.
func TestOneEnormousPromptCannotSwallowTheSection(t *testing.T) {
	huge := strings.Repeat("z", 40000)
	msgs := []bridgeMessage{{Role: "user", Content: huge}}

	prompts := sectionOf(t, buildDigest(sampleSession(), msgs, nil, digestCharBudget), "[USER PROMPTS")

	if strings.Contains(prompts, strings.Repeat("z", 2000)) {
		t.Fatalf("a %d-char prompt reached the digest unbounded (section is %d chars)", len(huge), len(prompts))
	}
	if !strings.Contains(prompts, "…") {
		t.Fatalf("the prompt was cut without saying so:\n%s", prompts[:min(300, len(prompts))])
	}
}

// "(none)" and an absent section are different statements. A session with no
// user prompts is a real and informative shape — a scheduled agent run nobody
// typed into — and the model is told to answer trackable=false to a session
// with no real work in it. An empty render leaves it to infer the difference
// between "nobody asked anything" and "the section was dropped to save space",
// which is the inference the whole per-section budget exists to remove.
func TestASessionWithNoUserPromptsSaysSoRatherThanRenderingNothing(t *testing.T) {
	msgs := []bridgeMessage{{Role: "assistant", Content: "started on my own"}}

	digest := buildDigest(sampleSession(), msgs, nil, digestCharBudget)

	if !strings.Contains(digest, "[USER PROMPTS") {
		t.Fatalf("the prompts section vanished instead of reporting itself empty:\n%s", digest)
	}
	if !strings.Contains(sectionOf(t, digest, "[USER PROMPTS"), "(none)") {
		t.Fatalf("an empty prompts section does not say (none):\n%s", sectionOf(t, digest, "[USER PROMPTS"))
	}
}

// budgetShare is the arithmetic the whole per-section reserve is built on. It
// is called with the caller's budget, not with the package default — the two
// are the same number on a production tick, which is why reading the constant
// instead of the argument is invisible until someone passes -digest-budget.
func TestEachSectionsReserveScalesWithTheBudgetItWasGivenNotTheDefault(t *testing.T) {
	if got := budgetShare(1000, 0.25); got != 250 {
		t.Errorf("budgetShare(1000, 0.25) = %d, want 250", got)
	}
	if got := budgetShare(0, 0.25); got != 0 {
		t.Errorf("budgetShare(0, 0.25) = %d, want 0", got)
	}

	// The observable end of the same thing: a digest built to a small explicit
	// budget must honor it. Reading digestCharBudget instead of charBudget
	// reserves the default share of a budget nobody asked for.
	const small = 3000
	digest := buildDigest(sampleSession(), longSession(40, 1400), nil, small)
	if len(digest) > small {
		t.Fatalf("digest is %d chars against an explicit budget of %d — a section reserved "+
			"against the default budget instead of the one it was given", len(digest), small)
	}
}

// totalLen decides whether the existing-cards section can afford bodies or
// must fall back to titles. It is measured in characters because the budget
// is; counting entries instead compares a card count against a character
// budget, which is always smaller, so bodies are always kept and the section
// always overruns.
func TestTheShedDecisionIsMeasuredInCharactersNotInCards(t *testing.T) {
	if got := totalLen([]string{"abc", "de"}); got != 5 {
		t.Fatalf("totalLen counted %d, want 5 characters across 2 entries", got)
	}
	if got := totalLen(nil); got != 0 {
		t.Fatalf("totalLen(nil) = %d, want 0", got)
	}

	// Three cards whose TITLES fit the reserve comfortably but whose BODIES do
	// not. Measured in characters the bodies are shed; counted as three
	// entries against a character budget, they never are.
	cards := []issuesclient.EntityCardEntry{
		card("card-1", "first", strings.Repeat("b", 4000)),
		card("card-2", "second", strings.Repeat("b", 4000)),
		card("card-3", "third", strings.Repeat("b", 4000)),
	}
	section := buildExistingCardsSection(cards, 1200)

	for _, id := range []string{"card-1", "card-2", "card-3"} {
		if !strings.Contains(section, id) {
			t.Errorf("card %s was dropped when shedding its body would have been enough", id)
		}
	}
	if strings.Contains(section, strings.Repeat("b", 500)) {
		t.Fatalf("card bodies survived a reserve they cannot fit in (section is %d chars against 1200)", len(section))
	}
}

// sectionOf returns one section of a digest, from its header to the start of
// the next one. Reading a section rather than the whole digest is what lets a
// test say "this is not in the prompts section" without a match elsewhere in
// the digest answering for it.
func sectionOf(t *testing.T, digest, header string) string {
	t.Helper()
	start := strings.Index(digest, header)
	if start < 0 {
		t.Fatalf("section %s missing from the digest:\n%s", header, digest)
	}
	rest := digest[start:]
	if next := strings.Index(rest[1:], "\n["); next >= 0 {
		return rest[:next+1]
	}
	return rest
}
