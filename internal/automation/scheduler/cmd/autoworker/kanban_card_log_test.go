package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanclient"
)

// TestAStrandedCardIsNamedAndNotCalledAFailure is the caller-side half of card
// a68ad12c. The library can carry the id in its error and it buys nothing if the
// one line that reaches the log still says "create card failed" and drops it.
func TestAStrandedCardIsNamedAndNotCalledAFailure(t *testing.T) {
	err := &kanbanclient.CardCreatedWithoutAllLinksError{
		CardID:     "f1107651-67dd-4be5-b03a-a9c964f5f4ce",
		EntityType: "session", EntityRef: "autoworker-anthropic-1783943268421792246",
		Cause: errors.New("400 Bad Request"),
	}
	line := cardCreationLogLine("anthropic", "autoworker-anthropic-1783943268421792246", "", err)

	// Strip the quoted error before looking for the id. The error's own text
	// names the card too, so a line that merely echoes %v would pass a plain
	// Contains check while the caller had in fact dropped the id it was handed —
	// two channels answering for each other. What is asserted here is that the
	// caller names the card IN ITS OWN RIGHT.
	withoutQuotedErr := strings.Replace(line, err.Error(), "<err>", 1)
	if !strings.Contains(withoutQuotedErr, "f1107651-67dd-4be5-b03a-a9c964f5f4ce") {
		t.Errorf("the line must name the stranded card itself, not only inside the error it quotes, got %q", line)
	}
	if withoutQuotedErr == line {
		t.Fatalf("expected the line to quote the error; it did not, so the check above proves nothing: %q", line)
	}
	if strings.Contains(line, "create card failed") {
		t.Errorf("the card WAS created; the line must not report a creation failure, got %q", line)
	}
	if !strings.Contains(line, "STRANDED") {
		t.Errorf("the line must mark the case as distinct from a plain failure, got %q", line)
	}
}

// TestARealCreationFailureStillReadsAsOne is the negative control. Without it,
// a line that said "STRANDED" for every error would pass the test above.
func TestARealCreationFailureStillReadsAsOne(t *testing.T) {
	line := cardCreationLogLine("anthropic", "sess-1", "", errors.New("500 Internal Server Error"))
	if !strings.Contains(line, "create card failed") {
		t.Errorf("a card that was never created must still read as a failure, got %q", line)
	}
	if strings.Contains(line, "STRANDED") {
		t.Errorf("nothing was stranded, got %q", line)
	}
}

// TestTheSuccessLineIsUnchanged pins the third branch, so the refactor that
// made this function testable cannot have quietly altered the quiet path.
func TestTheSuccessLineIsUnchanged(t *testing.T) {
	line := cardCreationLogLine("anthropic", "sess-1", "card-9", nil)
	for _, want := range []string{"card-9", kanbanColumnTarget, kanbanBoardName} {
		if !strings.Contains(line, want) {
			t.Errorf("success line missing %q, got %q", want, line)
		}
	}
	if strings.Contains(line, "STRANDED") || strings.Contains(line, "failed") {
		t.Errorf("success line must not read as an error, got %q", line)
	}
}
