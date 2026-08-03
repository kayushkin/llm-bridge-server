package store

import (
	"strings"
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
	// Each page must be no NEWER than the previous page's tail. "Strictly older"
	// is not the invariant even though it happens to hold for this fixture:
	// the sort key is (updated_at, session id), so a page boundary may legally
	// land inside a group of rows sharing one timestamp and the next page then
	// opens on that same timestamp. TestListSessionSummaries_TieGroupSpansPage
	// covers that case.
	if p2[0].UpdatedAt.After(p1[len(p1)-1].UpdatedAt) {
		t.Errorf("page2 head (%v) newer than page1 tail (%v)", p2[0].UpdatedAt, p1[len(p1)-1].UpdatedAt)
	}
}

// forceUpdatedAt pins a session's updated_at to an exact stored string, which is
// how a tie group is built deliberately. The literal shape is Go's t.String()
// format because that is what a bound time.Time writes through this driver, and
// what every row in the live table actually holds.
func forceUpdatedAt(t *testing.T, s *Store, sessionID, updatedAtText string) {
	t.Helper()
	res, err := s.db.Exec(
		`UPDATE sessions SET updated_at = ? WHERE COALESCE(NULLIF(session_id, ''), bridge_id) = ?`,
		updatedAtText, sessionID)
	if err != nil {
		t.Fatalf("force updated_at for %s: %v", sessionID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("force updated_at for %s affected %d rows, want 1", sessionID, n)
	}
}

// A page boundary landing INSIDE a group of rows that share one updated_at must
// not lose the rest of that group. This is the defect the compound cursor
// closes: with a timestamp-only cursor the strict `<` excluded the tied rows
// from the next page while the page size had already excluded them from this
// one, so they were returned by NO page and were unreachable by paging.
func TestListSessionSummaries_TieGroupSpansPage(t *testing.T) {
	s := testStore(t)
	// Five sessions; the middle three share a timestamp to the nanosecond.
	// newest ── t3 ── tie,tie,tie ── t1 ── oldest
	ids := []string{"z-newest", "tie-c", "tie-b", "tie-a", "a-oldest"}
	for _, id := range ids {
		mkSummarySession(t, s, id)
	}
	forceUpdatedAt(t, s, "z-newest", "2026-05-02 20:55:36.000000000 +0000 UTC")
	const tied = "2026-05-02 20:55:35.222618512 +0000 UTC"
	forceUpdatedAt(t, s, "tie-c", tied)
	forceUpdatedAt(t, s, "tie-b", tied)
	forceUpdatedAt(t, s, "tie-a", tied)
	forceUpdatedAt(t, s, "a-oldest", "2026-05-02 20:55:34.000000000 +0000 UTC")

	// Page size 2 puts the boundary after the first tied row, so the group is
	// split across the boundary — the exact shape that used to drop rows.
	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		rows, err := s.ListSessionSummaries(2, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			seen = append(seen, r.SessionID)
		}
		if len(rows) < 2 {
			break
		}
		cursor = rows[len(rows)-1].Cursor
	}

	if len(seen) != len(ids) {
		t.Fatalf("paged through %d rows (%v), want all %d — a tie group lost rows", len(seen), seen, len(ids))
	}
	got := map[string]int{}
	for _, id := range seen {
		got[id]++
	}
	for _, id := range ids {
		if got[id] != 1 {
			t.Errorf("session %q returned %d times across all pages, want exactly 1", id, got[id])
		}
	}
	// Within the tie group the order is by session id descending, which is what
	// makes the boundary reproducible rather than a coin toss.
	want := []string{"z-newest", "tie-c", "tie-b", "tie-a", "a-oldest"}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("page order[%d] = %q, want %q (full order %v)", i, seen[i], want[i], seen)
			break
		}
	}
}

// A tie group is only reachable if the cursor carries the id half. This is the
// control for the test above: it asserts the cursor's SHAPE, so a future change
// that silently drops the id half back out fails here with a clear reason
// rather than only as a mysterious row-count mismatch.
func TestSummaryCursor_CarriesBothHalves(t *testing.T) {
	s := testStore(t)
	mkSummarySession(t, s, "only")
	rows, err := s.ListSessionSummaries(10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	updatedAtText, sessionID := decodeSummaryCursor(rows[0].Cursor)
	if sessionID == "" {
		t.Fatalf("cursor %q carries no session id half", rows[0].Cursor)
	}
	if sessionID != "only" {
		t.Errorf("cursor id half = %q, want %q", sessionID, "only")
	}
	if updatedAtText == "" || !strings.Contains(updatedAtText, "UTC") {
		t.Errorf("cursor timestamp half = %q, want a stored t.String() timestamp", updatedAtText)
	}
}

// An id containing the separator must round-trip whole. Splitting on the LAST
// separator, or on every one, would truncate such an id into a cursor pointing
// at a session that does not exist — and the page after it would silently start
// in the wrong place.
func TestSummaryCursor_IDContainingSeparatorRoundTrips(t *testing.T) {
	const awkward = "sess|with|bars"
	updatedAtText, sessionID := decodeSummaryCursor(
		encodeSummaryCursor("2026-05-02 20:55:35.222618512 +0000 UTC", awkward))
	if sessionID != awkward {
		t.Errorf("id half = %q, want %q", sessionID, awkward)
	}
	if updatedAtText != "2026-05-02 20:55:35.222618512 +0000 UTC" {
		t.Errorf("timestamp half = %q", updatedAtText)
	}
}

// blankSessionID makes a row LEGACY: session_id empty, so its projected id comes
// from bridge_id via the COALESCE. Such rows exist in the live table's older
// range and are the reason the sort key is a named expression rather than the
// bare session_id column.
func blankSessionID(t *testing.T, s *Store, bridgeID string) {
	t.Helper()
	res, err := s.db.Exec(`UPDATE sessions SET session_id = '' WHERE bridge_id = ?`, bridgeID)
	if err != nil {
		t.Fatalf("blank session_id for %s: %v", bridgeID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("blank session_id for %s affected %d rows, want 1", bridgeID, n)
	}
}

// A legacy row inside a tie group must page like any other. Sorting by the bare
// session_id column while the cursor carries the COALESCEd id puts such a row in
// one place in the sort and looks for it in another — the same lost-row failure
// the compound cursor exists to close, reintroduced one layer down and only ever
// visible on rows the newest fixtures do not have.
func TestListSessionSummaries_TieGroupWithLegacyRow(t *testing.T) {
	s := testStore(t)
	ids := []string{"m-mid", "n-high", "l-low"}
	for _, id := range ids {
		mkSummarySession(t, s, id)
	}
	const tied = "2026-05-02 20:55:35.222618512 +0000 UTC"
	for _, id := range ids {
		forceUpdatedAt(t, s, id, tied)
	}
	// The MIDDLE row of the tie group is the legacy one, so a sort that reads
	// the bare column moves it to the end and the boundary lands elsewhere.
	blankSessionID(t, s, "m-mid")

	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		rows, err := s.ListSessionSummaries(1, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		seen = append(seen, rows[0].SessionID)
		cursor = rows[0].Cursor
	}

	want := []string{"n-high", "m-mid", "l-low"}
	if len(seen) != len(want) {
		t.Fatalf("paged %d rows (%v), want %d — a legacy row in a tie group was lost or repeated",
			len(seen), seen, len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("order = %v, want %v", seen, want)
		}
	}
}

// A cursor minted by the old server has no id half. It must keep the behaviour
// it was built for rather than erroring or being read as an id bound of "".
func TestListSessionSummaries_LegacyTimestampOnlyCursor(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"old-1", "old-2", "old-3"} {
		mkSummarySession(t, s, id)
	}
	forceUpdatedAt(t, s, "old-1", "2026-05-02 20:55:36.000000000 +0000 UTC")
	forceUpdatedAt(t, s, "old-2", "2026-05-02 20:55:35.000000000 +0000 UTC")
	forceUpdatedAt(t, s, "old-3", "2026-05-02 20:55:34.000000000 +0000 UTC")

	// Exactly what the previous server would have handed a client as `next`.
	legacy := "2026-05-02 20:55:35.000000000 +0000 UTC"
	rows, err := s.ListSessionSummaries(10, legacy)
	if err != nil {
		t.Fatalf("legacy cursor: %v", err)
	}
	if len(rows) != 1 || rows[0].SessionID != "old-3" {
		var got []string
		for _, r := range rows {
			got = append(got, r.SessionID)
		}
		t.Fatalf("legacy cursor returned %v, want [old-3] (strictly older than the cursor)", got)
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
