package store

import (
	"testing"
	"time"
)

func TestListSessionIDsUpdatedSince(t *testing.T) {
	s := testStore(t)
	s.CreateSession(&Session{SessionID: "br_w", Harness: "claude_code", State: "idle"})
	sessionIDs, err := s.ListSessionIDsUpdatedSince(time.Now().Add(-time.Minute))
	if err != nil || len(sessionIDs) != 1 || sessionIDs[0] != "br_w" {
		t.Fatalf("session made just now: got %v, %v; want [br_w]", sessionIDs, err)
	}
	sessionIDs, err = s.ListSessionIDsUpdatedSince(time.Now().Add(time.Minute))
	if err != nil || len(sessionIDs) != 0 {
		t.Fatalf("nothing updated after a future time: got %v, %v; want none", sessionIDs, err)
	}
}
