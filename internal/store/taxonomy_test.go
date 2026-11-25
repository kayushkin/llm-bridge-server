package store

import "testing"

func TestSessionClassCountsCountsEachTripleWithItsDates(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"br_a", "br_b"} {
		if err := s.CreateSession(&Session{SessionID: id, Harness: "claude_code", State: "idle", Type: "interactive", Purpose: "chat", Origin: "frontend"}); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := s.SessionClassCounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 1 || counts[0].Count != 2 || counts[0].Purpose != "chat" || counts[0].FirstSeen.IsZero() || counts[0].LastSeen.Before(counts[0].FirstSeen) {
		t.Fatalf("counts = %+v", counts)
	}
}
