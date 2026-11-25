package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanclient"
)

// TestRetiringACardRecordsWhatMovedSoTheMergeCanBeUndone pins the record that
// makes a merge reversible.
//
// The note used to say only where a card's evidence had gone, never which
// evidence it was. Undoing one bad member of a group then meant finding a board
// snapshot from before the pass ran — which is luck, not a procedure. Everything
// an undo needs (type and ref per link) has to be in the card it was taken from.
func TestRetiringACardRecordsWhatMovedSoTheMergeCanBeUndone(t *testing.T) {
	var patched map[string]any
	kanban := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &patched); err != nil {
				t.Errorf("PATCH body is not JSON: %v", err)
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer kanban.Close()

	kc := kanbanclient.New(kanban.URL, "")
	kc.BoardID = "board-1"

	moved := []kanbanclient.CardLink{
		{EntityType: linkTypeEmail, EntityRef: "imap-pretender:23166", Label: "Deals eligibility notice"},
		{EntityType: linkTypeEmailMessageID, EntityRef: "1516356665@wellsfargo.com"},
		{EntityType: linkTypeEmailSender, EntityRef: "wellsfargoonline@mail1.wellsfargo.com", Label: "Deals"},
	}
	loser := &cardState{ID: "loser-1", Title: "Wells Fargo Deals eligibility update", Body: "Prose about deals.\n\n" + evidenceMarker + "\n\n## Linked emails (1)\n"}
	survivor := &cardState{ID: "survivor-1", Title: "Wells Fargo bank account statements bucket"}

	if err := retireCard(config{}, kc, loser, survivor, moved); err != nil {
		t.Fatalf("retireCard: %v", err)
	}

	body, _ := patched["body"].(string)
	if body == "" {
		t.Fatal("retireCard wrote no body")
	}
	if got, want := patched["status"], "archived"; got != want {
		t.Errorf("status = %v, want %v — a retired card is archived, not deleted", got, want)
	}
	// Every link has to be recoverable: without both halves an undo cannot
	// re-attach it, and the type alone or the ref alone is not enough.
	for _, l := range moved {
		if !strings.Contains(body, l.EntityRef) {
			t.Errorf("the retire note does not name %q, so an undo cannot restore it", l.EntityRef)
		}
		if !strings.Contains(body, l.EntityType) {
			t.Errorf("the retire note does not give the entity type for %q", l.EntityRef)
		}
	}
	if !strings.Contains(body, survivor.ID) {
		t.Error("the retire note does not name the survivor, so an undo cannot find where the evidence went")
	}
	if !strings.Contains(body, "Prose about deals.") {
		t.Error("the card's own prose was destroyed; only its evidence section should be replaced")
	}
}

// TestRetiringACardWithNoLinksWritesNoEmptySection — a card can be merged away
// holding nothing, and an empty "Links moved (0)" heading is noise in a body a
// human only ever reads when something has gone wrong.
func TestRetiringACardWithNoLinksWritesNoEmptySection(t *testing.T) {
	if got := renderMovedLinks(nil); got != "" {
		t.Errorf("renderMovedLinks(nil) = %q, want empty", got)
	}
}

// TestTheMergePromptRefusesToGroupBySender guards the rule that a shared sender
// is not a shared bucket.
//
// This is the routing bug one level up. The model merged a Wells Fargo marketing
// card into a Wells Fargo statements card because "all are routine Wells Fargo
// account notifications" — grouping by who sent the mail rather than what it is,
// which is exactly what the filing path had just been fixed to stop doing.
func TestTheMergePromptRefusesToGroupBySender(t *testing.T) {
	for _, want := range []string{
		"SHARED SENDER IS NOT A SHARED BUCKET",
		"never by who sent it",
	} {
		if !strings.Contains(mergeSystemPrompt, want) {
			t.Errorf("the merge prompt no longer says %q; a sender-shaped merge would pass again", want)
		}
	}
}
