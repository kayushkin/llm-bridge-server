package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// Every per-field bound in the digest was already exercised by the suite and
// pinned by none of it.
//
// The existing tests move DIRECTIONS — first ten turns instead of the last
// ten, oldest prompts instead of newest, truncate stopping altogether — and
// the scorer's 129/130 is a statement about that case list. Measured 2026-08-14
// by the 148th nightly pass: seven separate boundary VALUES could each be moved
// to a different number with the whole suite staying green.
//
//	user-prompt cut     1500 -> 1900 and -> 400   UNNOTICED
//	assistant-turn cut   600 -> 900               UNNOTICED
//	card-body cut        400 -> 200               UNNOTICED
//	card-title cut        80 -> 40                UNNOTICED
//	assistant-turn cap    10 -> 11                UNNOTICED
//	tool-call cap         15 -> 16                UNNOTICED
//
// That is card 3c18632a's shape and it is why it is worth a file of its own: a
// suite can name an axis, document it, exercise the mechanism that guards it on
// every row, and still have every row land on the SAME SIDE of the boundary.
// The reason here is that every fixture is homogeneous. longSession(n, len)
// gives every message the same length, so a 40x1400 session puts every user
// prompt under 1500 and every assistant turn over 600 — the cut either never
// fires or always fires, and in both cases its exact value is invisible.
// TestOneEnormousPromptCannotSwallowTheSection comes closest and asserts only
// that no run of 2000 characters survives, which any cut below 2000 satisfies.
//
// Each test below straddles its boundary with two rows differing by one
// character, and marks the last character so the assertion can say WHICH side
// the cut fell on rather than only how long the result was. A length-only
// assertion cannot tell a correct cut from an off-by-one that trims from the
// wrong end.
//
// Ported 2026-09-25 from c6e9e67f. Main cuts with
// textcut.TruncateToByteBudgetOnRuneBoundary, whose bound is in BYTES and
// includes the ellipsis: an over-long value keeps bound-3 bytes and then "…".
// The branch's cut kept `bound` characters and added the ellipsis after them.
// Every fixture here is ASCII, so bytes and characters coincide.
//
// The numbers are written here as literals on purpose. Deriving a fixture from
// the constant under test makes it move with the code and pin nothing — the
// standing rule in scripts/sabotage-kanban-classifier.py's header.

// endMarked returns a string of exactly n characters whose last one is "Z", so
// a test can tell "this survived whole" from "this was cut" without counting.
func endMarked(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("a", n-1) + "Z"
}

// A user prompt is the only verbatim record of what was asked, so its bound is
// the one that decides how much of the user's own words the model ever sees.
func TestAUserPromptIsCutAtExactlyFifteenHundredCharacters(t *testing.T) {
	const bound = 1500
	msgs := []bridgeMessage{
		{Role: "user", Content: endMarked(bound)},
		{Role: "user", Content: endMarked(bound + 1)},
	}

	section := buildUserPromptsSection(msgs, 1_000_000)

	if !strings.Contains(section, endMarked(bound)) {
		t.Errorf("a prompt of exactly %d characters was cut; the bound is off by one on the low side", bound)
	}
	if strings.Contains(section, endMarked(bound+1)) {
		t.Errorf("a prompt of %d characters survived whole; the cut did not fire at %d", bound+1, bound)
	}
	// The over-long one must appear cut at the bound and say so, rather than
	// being dropped from the section altogether.
	wantCut := strings.Repeat("a", bound-len("…")) + "…"
	if !strings.Contains(section, wantCut) {
		t.Errorf("the %d-character prompt is not present cut to fit %d bytes with its ellipsis", bound+1, bound)
	}
}

// The assistant turns are the model's own record of what it did. Their bound is
// smaller than the prompts' because there are more of them.
func TestAnAssistantTurnIsCutAtExactlySixHundredCharacters(t *testing.T) {
	const bound = 600
	msgs := []bridgeMessage{
		{Role: "assistant", Content: endMarked(bound)},
		{Role: "assistant", Content: endMarked(bound + 1)},
	}

	section := buildAssistantTurnsSection(msgs, 1_000_000)

	if !strings.Contains(section, endMarked(bound)) {
		t.Errorf("a turn of exactly %d characters was cut; the bound is off by one on the low side", bound)
	}
	if strings.Contains(section, endMarked(bound+1)) {
		t.Errorf("a turn of %d characters survived whole; the cut did not fire at %d", bound+1, bound)
	}
	if want := strings.Repeat("a", bound-len("…")) + "…"; !strings.Contains(section, want) {
		t.Errorf("the %d-character turn is not present cut to fit %d bytes with its ellipsis", bound+1, bound)
	}
}

// The card-body bound is how much of an existing card the model is shown before
// it is asked to rewrite that card's body from a summary. It is the number
// residual fef4bb77 names as the reach of that data loss, so it is worth
// pinning independently of the shedding logic that may drop bodies entirely.
func TestAnExistingCardsBodyIsCutAtExactlyFourHundredCharacters(t *testing.T) {
	const bound = 400
	cards := []issuesclient.EntityCardEntry{
		card("card-fits", "fits", endMarked(bound)),
		card("card-over", "over", endMarked(bound+1)),
	}

	// A budget large enough that bodies are kept, so this test observes the
	// per-field cut and not the shed-to-titles decision above it.
	section := buildExistingCardsSection(cards, 1_000_000)

	if !strings.Contains(section, endMarked(bound)) {
		t.Errorf("a body of exactly %d characters was cut; the bound is off by one on the low side", bound)
	}
	if strings.Contains(section, endMarked(bound+1)) {
		t.Errorf("a body of %d characters survived whole; the cut did not fire at %d", bound+1, bound)
	}
	if want := strings.Repeat("a", bound-len("…")) + "…"; !strings.Contains(section, want) {
		t.Errorf("the %d-character body is not present cut to fit %d bytes with its ellipsis", bound+1, bound)
	}
}

// A card title is the identity the model matches on to decide whether a task is
// already filed, so a title cut shorter than the schema promises turns two
// different cards into one string. The schema/applier agreement is already
// scored; this pins the third site, the digest's own rendering.
func TestAnExistingCardsTitleIsCutAtExactlyEightyCharacters(t *testing.T) {
	const bound = 80
	cards := []issuesclient.EntityCardEntry{
		card("card-fits", endMarked(bound), "body"),
		card("card-over", endMarked(bound+1), "body"),
	}

	section := buildExistingCardsSection(cards, 1_000_000)

	if !strings.Contains(section, endMarked(bound)) {
		t.Errorf("a title of exactly %d characters was cut; the bound is off by one on the low side", bound)
	}
	if strings.Contains(section, endMarked(bound+1)) {
		t.Errorf("a title of %d characters survived whole; the cut did not fire at %d", bound+1, bound)
	}
	if want := strings.Repeat("a", bound-len("…")) + "…"; !strings.Contains(section, want) {
		t.Errorf("the %d-character title is not present cut to fit %d bytes with its ellipsis", bound+1, bound)
	}
}

// The turn cap is a spend control: it bounds how many assistant turns can reach
// the paid model regardless of how long the session ran. Asserting on the turns'
// CONTENT rather than on the rendered "turn N" numbering is deliberate — the
// section renumbers from 1 after slicing, so the numbering looks identical
// whether the cap kept ten or eleven.
func TestTheAssistantTurnCapKeepsExactlyTenTurns(t *testing.T) {
	const turnCap = 10

	atCap := make([]bridgeMessage, 0, turnCap)
	for i := 1; i <= turnCap; i++ {
		atCap = append(atCap, bridgeMessage{Role: "assistant", Content: fmt.Sprintf("turn-content-%02d", i)})
	}
	section := buildAssistantTurnsSection(atCap, 1_000_000)
	for i := 1; i <= turnCap; i++ {
		if want := fmt.Sprintf("turn-content-%02d", i); !strings.Contains(section, want) {
			t.Errorf("with exactly %d turns, %s was dropped — the turnCap fires one turn too early", turnCap, want)
		}
	}

	overCap := append(atCap, bridgeMessage{Role: "assistant", Content: fmt.Sprintf("turn-content-%02d", turnCap+1)})
	section = buildAssistantTurnsSection(overCap, 1_000_000)
	if strings.Contains(section, "turn-content-01") {
		t.Errorf("with %d turns the oldest survived — the turnCap does not fire at %d", turnCap+1, turnCap)
	}
	for i := 2; i <= turnCap+1; i++ {
		if want := fmt.Sprintf("turn-content-%02d", i); !strings.Contains(section, want) {
			t.Errorf("with %d turns, %s was dropped — the turnCap keeps fewer than %d", turnCap+1, want, turnCap)
		}
	}
}

// The tool turnCap is the same kind of spend control one section down, and it is
// the one section that is handed no budget at all — the turnCap is the only thing
// bounding it.
func TestTheToolActivityCapKeepsExactlyFifteenCalls(t *testing.T) {
	const toolCap = 15

	toolMsgs := func(n int) []bridgeMessage {
		events := []map[string]any{}
		for i := 1; i <= n; i++ {
			events = append(events, map[string]any{
				"type":      "tool_call",
				"tool_name": fmt.Sprintf("tool-%02d", i),
			})
		}
		return []bridgeMessage{{Role: "assistant", Events: events}}
	}

	section := buildToolActivitySection(toolMsgs(toolCap))
	for i := 1; i <= toolCap; i++ {
		if want := fmt.Sprintf("- tool-%02d\n", i); !strings.Contains(section, want) {
			t.Errorf("with exactly %d tool calls, tool-%02d was dropped — the toolCap fires one call too early", toolCap, i)
		}
	}

	section = buildToolActivitySection(toolMsgs(toolCap + 1))
	if strings.Contains(section, "- tool-01\n") {
		t.Errorf("with %d tool calls the oldest survived — the toolCap does not fire at %d", toolCap+1, toolCap)
	}
	for i := 2; i <= toolCap+1; i++ {
		if want := fmt.Sprintf("- tool-%02d\n", i); !strings.Contains(section, want) {
			t.Errorf("with %d tool calls, tool-%02d was dropped — the toolCap keeps fewer than %d", toolCap+1, i, toolCap)
		}
	}
}
