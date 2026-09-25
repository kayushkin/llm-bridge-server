package main

import (
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/degradations"
	"github.com/slava/scheduler/internal/kanbanclient"
)

// Ported from 0530193e (test/first-tests-for-kanban-classifier). applyNew's title
// dedupe is what stops a session re-filing work it has already filed — the
// defect that once minted six cards for one task in ninety minutes.

func linkedCard(id, title string) kanbanclient.EntityCardEntry {
	return kanbanclient.EntityCardEntry{
		CardID: id,
		Item:   map[string]any{"title": title, "status": "open", "body": "b"},
	}
}

// The digest is what really prevents re-filing; this is the last line of
// defence, and the one that fires when the digest has already failed.
func TestANewTaskWhoseTitleIsAlreadyOnTheBoardIsNotFiledAgain(t *testing.T) {
	kc, rec := newStubKanban(t)

	applyNew(kc, testSession(),
		[]kanbanclient.EntityCardEntry{linkedCard("card-1", "Build the split view")},
		&Report{NewTasks: []NewRec{
			{Title: "build the split view!", State: "in_progress", Summary: "ALREADY-FILED"},
			{Title: "Something genuinely new", State: "in_progress", Summary: "GENUINELY-NEW"},
		}}, degradations.New("classifier"))

	if len(rec.createdBodies) != 1 || !strings.Contains(rec.createdBodies[0], "GENUINELY-NEW") {
		t.Fatalf("created %q, want only the genuinely new task — the other is already on the board", rec.createdBodies)
	}
}

// The dedupe also has to hold WITHIN one report. A model that lists the same task
// twice is exactly the failure the dedupe exists for, and the existing cards it is
// compared against are empty on a session's first classification.
func TestOneReportNamingTheSameTaskTwiceFilesItOnce(t *testing.T) {
	kc, rec := newStubKanban(t)

	applyNew(kc, testSession(), nil, &Report{NewTasks: []NewRec{
		{Title: "Port the resolver", State: "in_progress", Summary: "first"},
		{Title: "port the resolver.", State: "blocked", Summary: "second"},
	}}, degradations.New("classifier"))

	if len(rec.createdBodies) != 1 {
		t.Fatalf("created %d cards for one task: %q", len(rec.createdBodies), rec.createdBodies)
	}
}

// The session link is the join; this one says who wrote the card. Without it a
// classifier card cannot be told from one a person filed except by reading prose.
func TestACreatedCardIsMarkedAsClassifierWritten(t *testing.T) {
	kc, rec := newStubKanban(t)

	applyNew(kc, testSession(), nil, &Report{NewTasks: []NewRec{
		{Title: "Port the resolver", State: "in_progress", Summary: "s"},
	}}, degradations.New("classifier"))

	want := cardLinkSource + "=" + cardSourceClassifier
	if !contains(rec.links, want) {
		t.Errorf("missing link %q among %v", want, rec.links)
	}
}

// An unknown state is a model that answered off-schema. In Progress is the safe
// landing: it keeps the card visible and in play rather than filing it as
// finished or failed on a guess.
func TestAnUnrecognisedStateLandsInProgressRatherThanBeingJudged(t *testing.T) {
	for _, state := range []string{"", "in progress", "IN_PROGRESS", "cancelled", "wontfix"} {
		if got := stateToColumn(state); got != "In Progress" {
			t.Errorf("stateToColumn(%q) = %q, want In Progress", state, got)
		}
	}
}

// The dedupe key folds punctuation and case. Digits must survive it, or two tasks
// that differ only by a number become one and the second is never filed.
func TestTitlesDifferingOnlyByANumberDoNotDedupeIntoOne(t *testing.T) {
	if normalizeTitle("Fix bug 1") == normalizeTitle("Fix bug 2") {
		t.Fatal("two tasks differing only by a number normalize equal, so the second is never filed")
	}
}
