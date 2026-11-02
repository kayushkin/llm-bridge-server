package store

import (
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestSessionBriefsAreNarrowedToTheOwnersSessions(t *testing.T) {
	s := testStore(t)
	for _, session := range []*Session{
		{SessionID: "br_mine", Harness: "claude_code", State: "idle", PrincipalID: "principal_000001"},
		{SessionID: "br_theirs", Harness: "claude_code", State: "idle", PrincipalID: "principal_000002"},
	} {
		if err := s.CreateSession(session); err != nil {
			t.Fatal(err)
		}
		if err := s.ApplySessionBriefUpdate(SessionBriefUpdate{
			SessionID: session.SessionID,
			Revision:  msg.SessionBriefRevision{ID: "sbrev_" + session.SessionID, OverallGoal: "goal of " + session.SessionID},
			Raised:    []msg.LooseEnd{{ID: "loose_" + session.SessionID, Kind: msg.LooseEndQuestion, Text: "q"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	views, err := s.ListSessionBriefs(SessionBriefFilter{OwnedByPrincipalID: "principal_000001"})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Brief.SessionID != "br_mine" || len(views[0].LooseEnds) != 1 {
		t.Fatalf("views = %+v; want only br_mine with its loose end", views)
	}
	all, _ := s.ListSessionBriefs(SessionBriefFilter{})
	if len(all) != 2 {
		t.Errorf("unnarrowed = %d briefs, want 2", len(all))
	}
}

func TestALooseEndClosesOnce(t *testing.T) {
	s := testStore(t)
	if err := s.ApplySessionBriefUpdate(SessionBriefUpdate{
		SessionID: "br_1",
		Revision:  msg.SessionBriefRevision{ID: "sbrev_1", OverallGoal: "g"},
		Raised:    []msg.LooseEnd{{ID: "loose_1", Kind: msg.LooseEndOffer, Text: "t"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseLooseEnd("loose_1", msg.LooseEndAnswered, "yes"); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseLooseEnd("loose_1", msg.LooseEndDismissed, ""); !errors.Is(err, ErrLooseEndNotOpen) {
		t.Errorf("second close = %v, want ErrLooseEndNotOpen", err)
	}
	// A later turn settling it must not overwrite the answer.
	if err := s.ApplySessionBriefUpdate(SessionBriefUpdate{
		SessionID: "br_1",
		Revision:  msg.SessionBriefRevision{ID: "sbrev_2", OverallGoal: "g"},
		Settled:   map[string]string{"loose_1": "later turn"},
	}); err != nil {
		t.Fatal(err)
	}
	looseEnd, _ := s.GetLooseEnd("loose_1")
	if looseEnd.State != msg.LooseEndAnswered || looseEnd.Resolution != "yes" {
		t.Errorf("loose end = %+v, want the answer kept", looseEnd)
	}
}
