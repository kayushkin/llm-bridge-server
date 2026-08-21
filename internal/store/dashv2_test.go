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

	rows, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{})
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
	p1, err := s.ListSessionSummaries(2, "", SessionSummaryFilter{})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(p1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(p1))
	}
	// Page 2: older than the last of page 1.
	p2, err := s.ListSessionSummaries(2, p1[len(p1)-1].Cursor, SessionSummaryFilter{})
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
		rows, err := s.ListSessionSummaries(2, cursor, SessionSummaryFilter{})
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
	rows, err := s.ListSessionSummaries(10, "", SessionSummaryFilter{})
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
		rows, err := s.ListSessionSummaries(1, cursor, SessionSummaryFilter{})
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
	rows, err := s.ListSessionSummaries(10, legacy, SessionSummaryFilter{})
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

// mkFilterableSession creates a session with the axes the filter reads set
// independently, so a test can tell which axis did the narrowing.
func mkFilterableSession(t *testing.T, s *Store, id, sessionType, purpose string) {
	t.Helper()
	sess := &Session{
		SessionID:   id,
		DisplayName: "disp-" + id,
		Harness:     "claude_code",
		InstanceID:  "inst-1",
		State:       "idle",
		AgentID:     "agent-1",
		Purpose:     purpose,
		Type:        msg.SessionType(sessionType),
		Mode:        msg.SessionModeEvents,
	}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

// ids reports the session ids of a page, in order, for comparison in tests.
func ids(rows []SessionSummaryRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SessionID)
	}
	return out
}

// A zero-value filter constrains nothing — the pre-filter behaviour, unchanged.
func TestListSessionSummaries_EmptyFilterListsEverything(t *testing.T) {
	s := testStore(t)
	mkFilterableSession(t, s, "a", "interactive", "chat")
	mkFilterableSession(t, s, "b", "autonomous", "autoworker")

	rows, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{})
	if err != nil {
		t.Fatalf("ListSessionSummaries: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("empty filter must list everything, got %v", ids(rows))
	}
	if !(SessionSummaryFilter{}).IsEmpty() {
		t.Errorf("zero-value filter must report IsEmpty")
	}
}

// One axis narrows to the rows matching it, and drops the rest entirely.
func TestListSessionSummaries_FilterNarrowsToMatchingAxis(t *testing.T) {
	s := testStore(t)
	mkFilterableSession(t, s, "chat-1", "interactive", "chat")
	mkFilterableSession(t, s, "worker-1", "autonomous", "autoworker")
	mkFilterableSession(t, s, "worker-2", "autonomous", "autoworker")

	rows, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{Types: []string{"interactive"}})
	if err != nil {
		t.Fatalf("ListSessionSummaries: %v", err)
	}
	if got := ids(rows); len(got) != 1 || got[0] != "chat-1" {
		t.Errorf("type=interactive should yield only chat-1, got %v", got)
	}
}

// Values within one axis are OR'd — the sidebar's inclusion semantics.
func TestListSessionSummaries_AxisValuesAreOred(t *testing.T) {
	s := testStore(t)
	mkFilterableSession(t, s, "chat-1", "interactive", "chat")
	mkFilterableSession(t, s, "herald-1", "herald", "herald")
	mkFilterableSession(t, s, "worker-1", "autonomous", "autoworker")

	rows, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{
		Types: []string{"interactive", "herald"},
	})
	if err != nil {
		t.Fatalf("ListSessionSummaries: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("two values on one axis must OR, got %v", ids(rows))
	}
}

// Separate axes are AND'd — a row has to satisfy every non-empty one.
func TestListSessionSummaries_AxesAreAnded(t *testing.T) {
	s := testStore(t)
	mkFilterableSession(t, s, "match", "interactive", "chat")
	mkFilterableSession(t, s, "wrong-purpose", "interactive", "autoworker")
	mkFilterableSession(t, s, "wrong-type", "autonomous", "chat")

	rows, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{
		Types:    []string{"interactive"},
		Purposes: []string{"chat"},
	})
	if err != nil {
		t.Fatalf("ListSessionSummaries: %v", err)
	}
	if got := ids(rows); len(got) != 1 || got[0] != "match" {
		t.Errorf("axes must AND, got %v", got)
	}
}

// THE regression this endpoint is most likely to grow: a filter that holds on
// page one and silently stops holding from page two onwards.
//
// The cursor test is an OR (`older-timestamp OR same-timestamp-and-lower-id`).
// Appended to it, an unparenthesized ` AND type IN (…)` binds as
// `A OR (B AND C AND type)`, so every row older than the cursor comes back
// whatever its type. Page one carries no cursor and looks perfectly correct,
// which is exactly why this needs a test rather than a reading.
func TestListSessionSummaries_FilterSurvivesCursorPagination(t *testing.T) {
	s := testStore(t)
	// Interleaved so any page after the first would pick up an autonomous row
	// if the filter stopped applying.
	for _, spec := range []struct{ id, sessionType string }{
		{"i1", "interactive"},
		{"a1", "autonomous"},
		{"i2", "interactive"},
		{"a2", "autonomous"},
		{"i3", "interactive"},
		{"a3", "autonomous"},
	} {
		mkFilterableSession(t, s, spec.id, spec.sessionType, "chat")
		time.Sleep(2 * time.Millisecond)
	}

	filter := SessionSummaryFilter{Types: []string{"interactive"}}
	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		rows, err := s.ListSessionSummaries(2, cursor, filter)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if r.Type != "interactive" {
				t.Fatalf("page %d leaked a %q row (%s) — the filter stopped applying past the cursor",
					page, r.Type, r.SessionID)
			}
			seen = append(seen, r.SessionID)
		}
		cursor = rows[len(rows)-1].Cursor
	}
	if len(seen) != 3 {
		t.Errorf("paging a filtered list must reach every match exactly once, got %v", seen)
	}
}

// The response cache is keyed by this string, so two different filters must
// never render the same one — that would serve one filter's page to another.
func TestSessionSummaryFilterCacheKey_DistinguishesFilters(t *testing.T) {
	a := SessionSummaryFilter{Types: []string{"interactive"}}
	b := SessionSummaryFilter{Types: []string{"autonomous"}}
	if a.CacheKey() == b.CacheKey() {
		t.Errorf("different filters share a cache key: %q", a.CacheKey())
	}

	// Same axis, same values, different axis entirely — must not collide either.
	c := SessionSummaryFilter{Purposes: []string{"interactive"}}
	if a.CacheKey() == c.CacheKey() {
		t.Errorf("type and purpose filters share a cache key: %q", a.CacheKey())
	}

	// Chip order is not part of the selection, so it must not split the cache.
	d := SessionSummaryFilter{Types: []string{"herald", "interactive"}}
	e := SessionSummaryFilter{Types: []string{"interactive", "herald"}}
	if d.CacheKey() != e.CacheKey() {
		t.Errorf("value order changed the cache key: %q vs %q", d.CacheKey(), e.CacheKey())
	}
}

// CacheKey must not reorder the caller's slice as a side effect — the same
// filter value is handed to the SQL builder immediately afterwards.
func TestSessionSummaryFilterCacheKey_DoesNotMutateCaller(t *testing.T) {
	values := []string{"interactive", "herald"}
	f := SessionSummaryFilter{Types: values}
	_ = f.CacheKey()
	if values[0] != "interactive" || values[1] != "herald" {
		t.Errorf("CacheKey sorted the caller's slice in place: %v", values)
	}
}

// TestListSessionSummariesBySessionIDs pins the id LOOKUP: give it ids, get
// exactly those rows, whatever their position in the recency order.
//
// The point is reachability, not filtering. dashv2's signals inbox holds ids it
// got from the signals table and needs the names behind them; on this host most
// of those sessions are thousands of rows deep in a listing ordered by
// updated_at, so no page size the sidebar would ever ask for reaches them.
func TestListSessionSummariesBySessionIDs(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"br_a", "br_b", "br_c", "br_d"} {
		if err := s.CreateSession(&Session{SessionID: id, Harness: "claude_code", State: "idle", DisplayName: "name " + id}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	got, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{SessionIDs: []string{"br_a", "br_c"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want the 2 asked for", len(got))
	}
	names := map[string]string{}
	for _, r := range got {
		names[r.SessionID] = r.DisplayName
	}
	if names["br_a"] != "name br_a" || names["br_c"] != "name br_c" {
		t.Errorf("the lookup did not carry the display names back: %+v", names)
	}

	// An id nobody has is simply absent — not an error, and not a listing. A
	// caller holding a stale id must get back "no such session", not every
	// session in the store.
	got, err = s.ListSessionSummaries(100, "", SessionSummaryFilter{SessionIDs: []string{"br_gone"}})
	if err != nil {
		t.Fatalf("list unknown: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an unknown id returned %d rows, want 0", len(got))
	}
}

// TestSessionSummaryFilterKeepsTheLookupOutOfTheChipAxes pins the separation.
//
// axes() is documented as mirroring the sidebar's six filter chips, and it
// drives both the SQL and the cache key. If the id lookup were folded in as a
// seventh, every consumer that reasons about "which chips are set" would start
// counting a lookup among them.
//
// IsEmpty must still report false for a lookup-only filter, because it decides
// whether a request is narrowed at all — and a request narrowed to two rows is
// emphatically narrowed.
func TestSessionSummaryFilterKeepsTheLookupOutOfTheChipAxes(t *testing.T) {
	lookup := SessionSummaryFilter{SessionIDs: []string{"br_a"}}
	for _, axis := range lookup.axes() {
		if len(axis.values) > 0 {
			t.Errorf("the id lookup leaked into chip axis %q", axis.column)
		}
	}
	if lookup.IsEmpty() {
		t.Error("a filter narrowed to one session reports as constraining nothing")
	}

	// And it has to reach the cache key, or a page cached for one set of ids
	// gets served for another — a wrong answer that looks exactly like a right
	// one.
	other := SessionSummaryFilter{SessionIDs: []string{"br_b"}}
	if lookup.CacheKey() == other.CacheKey() {
		t.Error("two different id lookups share a cache key")
	}
	if lookup.CacheKey() == (SessionSummaryFilter{}).CacheKey() {
		t.Error("a lookup and an unfiltered listing share a cache key")
	}
	// Order must not matter: the same set asked for twice is one entry.
	if (SessionSummaryFilter{SessionIDs: []string{"br_a", "br_b"}}).CacheKey() !=
		(SessionSummaryFilter{SessionIDs: []string{"br_b", "br_a"}}).CacheKey() {
		t.Error("the same id set in a different order builds two cache keys")
	}
}

// TestListSessionSummariesByManagerSessionIDs pins the PARENT lookup: give it
// parents, get their children, whatever their position in the recency order.
//
// The point is the same reachability the id lookup above exists for, and the
// numbers are why it cannot be done client-side. On this host 1,325 subagent
// sessions hang off 435 parents, one of them with 106 children — and a child is
// ordered by its OWN updated_at, not its parent's, so a sidebar that grouped
// only the children on its loaded page would show four of that 106 and say
// nothing about the other 102.
//
// Several parents in one call is deliberate: the sidebar asks about a whole page
// of rows in ONE request. That is also what lets it know WHICH rows have
// children at all, without a count column existing anywhere.
func TestListSessionSummariesByManagerSessionIDs(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"br_parent", "br_other_parent", "br_unrelated"} {
		if err := s.CreateSession(&Session{SessionID: id, Harness: "claude_code", State: "idle", DisplayName: "name " + id}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	children := map[string]string{
		"br_kid_1": "br_parent",
		"br_kid_2": "br_parent",
		"br_kid_3": "br_other_parent",
	}
	for id, parent := range children {
		if err := s.CreateSession(&Session{
			SessionID: id, Harness: "claude_code", State: "idle",
			DisplayName: "name " + id, ManagerSessionID: parent,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	got, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{
		ManagerSessionIDs: []string{"br_parent"},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want br_parent's 2 children", len(got))
	}
	for _, row := range got {
		if row.ManagerSessionID != "br_parent" {
			t.Errorf("row %s has manager %q, want br_parent", row.SessionID, row.ManagerSessionID)
		}
	}

	// Two parents at once — the sidebar's real shape.
	got, err = s.ListSessionSummaries(100, "", SessionSummaryFilter{
		ManagerSessionIDs: []string{"br_parent", "br_other_parent"},
	})
	if err != nil {
		t.Fatalf("list two parents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows across two parents, want 3", len(got))
	}

	// A parent with no children is an empty answer, never every session. This is
	// the case that would silently become "here is the whole box" if the lookup
	// were dropped when it matched nothing.
	got, err = s.ListSessionSummaries(100, "", SessionSummaryFilter{
		ManagerSessionIDs: []string{"br_unrelated"},
	})
	if err != nil {
		t.Fatalf("list childless: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows for a session that spawned nothing, want 0", len(got))
	}
}

// TestSessionSummaryRowCarriesManagerSessionID pins the field on the PROJECTION,
// which is the half that was missing.
//
// `manager_session_id` has always been on the full session record, so
// `GET /sessions/{id}` and every SSE upsert carried it. The paged list did not,
// and the sidebar reads only the list — so it could not tell a parent from a
// child however many rows it loaded. A test that only exercised the lookup above
// would still pass with the column absent from the projection, because the
// filter reads the table directly.
func TestSessionSummaryRowCarriesManagerSessionID(t *testing.T) {
	s := testStore(t)
	if err := s.CreateSession(&Session{SessionID: "br_top", Harness: "claude_code", State: "idle", DisplayName: "top"}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if err := s.CreateSession(&Session{
		SessionID: "br_child", Harness: "claude_code", State: "idle",
		DisplayName: "child", ManagerSessionID: "br_top",
	}); err != nil {
		t.Fatalf("create child: %v", err)
	}

	rows, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]SessionSummaryRow{}
	for _, r := range rows {
		byID[r.SessionID] = r
	}
	if got := byID["br_child"].ManagerSessionID; got != "br_top" {
		t.Errorf("child's ManagerSessionID = %q, want br_top", got)
	}
	// Empty, never a placeholder: a top-level session HAS no parent, and
	// inventing one would make every root look like somebody's child.
	if got := byID["br_top"].ManagerSessionID; got != "" {
		t.Errorf("top-level ManagerSessionID = %q, want empty", got)
	}
}

// TestManagerLookupRidesTheCacheKey pins the cache key. Two requests differing
// only by which parent they ask about share a revision, a limit and a cursor, so
// without the key one parent's children would be served as another's — a wrong
// answer that looks exactly like a right one.
func TestManagerLookupRidesTheCacheKey(t *testing.T) {
	a := SessionSummaryFilter{ManagerSessionIDs: []string{"br_parent"}}
	b := SessionSummaryFilter{ManagerSessionIDs: []string{"br_other"}}
	if a.CacheKey() == b.CacheKey() {
		t.Fatal("two different parents produced the same cache key")
	}
	if (SessionSummaryFilter{}).CacheKey() == a.CacheKey() {
		t.Fatal("a parent lookup keyed the same as no lookup at all")
	}
	// Order must not matter — the sidebar builds this list from a page whose
	// order is its own business.
	if (SessionSummaryFilter{ManagerSessionIDs: []string{"a", "b"}}).CacheKey() !=
		(SessionSummaryFilter{ManagerSessionIDs: []string{"b", "a"}}).CacheKey() {
		t.Fatal("cache key depends on the order the parents were listed in")
	}
	// And a parent lookup is not "unfiltered".
	if (SessionSummaryFilter{ManagerSessionIDs: []string{"br_parent"}}).IsEmpty() {
		t.Fatal("a parent lookup reported itself as constraining nothing")
	}
}
