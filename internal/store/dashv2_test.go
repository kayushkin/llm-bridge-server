package store

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// mkSummarySession creates a session with the projected fields populated.
func mkSummarySession(t *testing.T, s *Store, id string) {
	t.Helper()
	sess := &Session{
		SessionID:   id,
		DisplayName: "disp-" + id,
		Harness:     "claude_code",
		InstanceID:  "inst-1",
		State:       "idle",
		AgentID:     "agent-1",
		Purpose:     "chat",
		Type:        msg.SessionType("interactive"),
		Mode:        msg.SessionModeEvents,
	}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

// The projected list is newest-first and carries only the light columns.
func TestListSessionSummaries_NewestFirstProjection(t *testing.T) {
	s := testStore(t)
	mkSummarySession(t, s, "a")
	time.Sleep(2 * time.Millisecond)
	mkSummarySession(t, s, "b")
	time.Sleep(2 * time.Millisecond)
	mkSummarySession(t, s, "c")
	// Bump 'a' so it becomes the most recently updated.
	time.Sleep(2 * time.Millisecond)
	if err := s.UpdateSessionState("a", "running"); err != nil {
		t.Fatalf("update: %v", err)
	}

	rows, err := s.ListSessionSummaries(100, "")
	if err != nil {
		t.Fatalf("ListSessionSummaries: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	if rows[0].SessionID != "a" {
		t.Errorf("newest-first: expected 'a' first (just updated), got %q", rows[0].SessionID)
	}
	// Projected fields present.
	r := rows[0]
	if r.DisplayName != "disp-a" || r.Harness != "claude_code" || r.State != "running" ||
		r.InstanceID != "inst-1" || r.AgentID != "agent-1" || r.Purpose != "chat" ||
		r.Type != "interactive" {
		t.Errorf("projected fields wrong: %+v", r)
	}
	if r.UpdatedAt.IsZero() || r.CreatedAt.IsZero() {
		t.Errorf("timestamps must be populated: %+v", r)
	}
	if r.Cursor == "" {
		t.Errorf("cursor (raw updated_at) must be set")
	}
}

// The cursor paginates strictly older, with no overlap and no gaps.
func TestListSessionSummaries_CursorPagination(t *testing.T) {
	s := testStore(t)
	ids := []string{"s0", "s1", "s2", "s3", "s4"}
	for _, id := range ids {
		mkSummarySession(t, s, id)
		time.Sleep(2 * time.Millisecond)
	}
	// Page 1: newest 2.
	p1, err := s.ListSessionSummaries(2, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(p1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(p1))
	}
	// Page 2: older than the last of page 1.
	p2, err := s.ListSessionSummaries(2, p1[len(p1)-1].Cursor)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(p2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(p2))
	}
	seen := map[string]bool{}
	for _, r := range append(append([]SessionSummaryRow{}, p1...), p2...) {
		if seen[r.SessionID] {
			t.Errorf("duplicate across pages: %s", r.SessionID)
		}
		seen[r.SessionID] = true
	}
	// Each page must be strictly older than the previous page's tail.
	if !(p2[0].UpdatedAt.Before(p1[len(p1)-1].UpdatedAt)) {
		t.Errorf("page2 head (%v) not older than page1 tail (%v)", p2[0].UpdatedAt, p1[len(p1)-1].UpdatedAt)
	}
}

// The revision is the newest updated_at and moves when a row is touched.
func TestMaxSessionUpdatedAt_TracksNewest(t *testing.T) {
	s := testStore(t)
	if rev, err := s.MaxSessionUpdatedAt(); err != nil || rev != "" {
		t.Fatalf("empty table revision = %q, err %v; want empty", rev, err)
	}
	mkSummarySession(t, s, "x")
	rev1, err := s.MaxSessionUpdatedAt()
	if err != nil || rev1 == "" {
		t.Fatalf("revision after insert = %q, err %v", rev1, err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.UpdateSessionState("x", "running"); err != nil {
		t.Fatalf("update: %v", err)
	}
	rev2, err := s.MaxSessionUpdatedAt()
	if err != nil {
		t.Fatalf("rev2: %v", err)
	}
	if rev2 == rev1 {
		t.Errorf("revision did not advance after mutation: %q", rev2)
	}
}
