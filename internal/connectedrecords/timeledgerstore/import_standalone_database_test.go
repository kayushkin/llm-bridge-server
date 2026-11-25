package timeledgerstore

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"

	_ "modernc.org/sqlite"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store
// kept one, with a few made-up rows, gaps in every numbering, a child entry
// numbered before its parent, and an AUTOINCREMENT counter past the highest
// row left.
func newStandaloneDatabase(t *testing.T) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "time-ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	entry := `INSERT INTO time_entries (seq, id, title, title_by, title_by_kind, span_by, span_by_kind, starts_at, ends_at, all_day, start_date, end_date,
		time_zone, place, place_by, place_by_kind, parent_time_entry_id, parent_set_by, parent_set_by_kind, created_by, created_by_kind, created_reason,
		created_at, updated_at, evidence_changed_at, deleted_at, deleted_by) VALUES `
	statements := []string{
		`PRAGMA foreign_keys = OFF`,
		entry + `(2, 'time_entry_000002', 'Workshop', 'import', 'program', 'import', 'program', 2000, 3000, 0, '', '', 'Europe/Lisbon', 'Hall', 'import', 'program',
			'time_entry_000005', 'import', 'program', 'import', 'program', 'calendar', 100, 200, 200, NULL, '')`,
		entry + `(5, 'time_entry_000005', 'A week away', 'me', 'operator', 'import', 'program', 1000, 9000, 1, '2024-03-01', '2024-03-08', 'Europe/Lisbon', '', '', '',
			NULL, '', '', 'import', 'program', 'journey', 150, 300, 300, NULL, '')`,
		entry + `(6, 'time_entry_000006', 'Skipped class', 'import', 'program', 'import', 'program', 4000, 5000, 0, '', '', 'UTC', '', '', '',
			NULL, '', '', 'import', 'program', 'calendar', 160, 400, 160, 400, 'me')`,
		`INSERT INTO time_entry_revisions (revision_id, time_entry_id, reason, replaced_at, entry) VALUES (3, 'time_entry_000005', 'renamed', 300, '{"id":"time_entry_000005","title":"Away"}')`,
		`INSERT INTO time_entry_kinds (seq, id, name, description, created_by, created_by_kind, created_at) VALUES (1, 'time_entry_kind_000001', 'journey', 'away', 'seed', 'program', 50)`,
		`INSERT INTO time_entry_kinds (seq, id, name, description, created_by, created_by_kind, created_at) VALUES (4, 'time_entry_kind_000004', 'class', 'lesson', 'me', 'operator', 60)`,
		`INSERT INTO time_entry_kind_assignments (seq, time_entry_id, kind_id, assigned_by, assigned_by_kind, reason, assigned_at, removed_at, removed_by, removed_by_kind)
			VALUES (7, 'time_entry_000005', 'time_entry_kind_000001', 'import', 'program', 'journey', 150, NULL, '', '')`,
		`INSERT INTO time_entry_kind_assignments (seq, time_entry_id, kind_id, assigned_by, assigned_by_kind, reason, assigned_at, removed_at, removed_by, removed_by_kind)
			VALUES (8, 'time_entry_000006', 'time_entry_kind_000004', 'review', 'model', 'a lesson', 170, 180, 'me', 'operator')`,
		`INSERT INTO time_entry_reviews (seq, id, time_entry_id, outcome, reason, reviewed_by, reviewed_by_kind, model, input_tokens, output_tokens, cost_usd,
			links_seen, suggested_parent_time_entry_id, suggested_kind_names, created_at)
			VALUES (9, 'time_entry_review_000009', 'time_entry_000006', 'skipped', 'no visit', 'review', 'model', 'claude-opus-5-5', 1200, 80, 0.0375,
			'[{"link_type":"visits"}]', 'time_entry_000005', '["class"]', 500)`,
		`INSERT INTO time_entry_calendar_events (seq, time_entry_id, calendar_event_id, occurrence_starts_at, bears_on, linked_by, linked_by_kind, reason, linked_at)
			VALUES (3, 'time_entry_000002', 'calendar_event_000012', 2000, 'source', 'import', 'program', 'calendar date', 100)`,
		`INSERT INTO time_entry_trips (seq, time_entry_id, trip_id, overlap_seconds, bears_on, linked_by, linked_by_kind, reason, linked_at)
			VALUES (2, 'time_entry_000005', 'trip_000040', 8000, 'source', 'import', 'program', 'journey', 150)`,
		`INSERT INTO time_entry_transactions (seq, time_entry_id, transaction_id, amount_cents, bears_on, linked_by, linked_by_kind, reason, linked_at, removed_at, removed_by, removed_by_kind)
			VALUES (4, 'time_entry_000005', 'transaction_000077', 2350, 'cost', 'gather', 'program', 'hotel', 160, 170, 'gather', 'program')`,
		`INSERT INTO time_entry_visits (seq, time_entry_id, visit_id, overlap_seconds, bears_on, linked_by, linked_by_kind, reason, linked_at)
			VALUES (1, 'time_entry_000006', 'visit_000003', 0, 'context', 'gather', 'program', 'nearby', 165)`,
		// The counter SQLite keeps is past the highest entry left: entry 8
		// was handed out once, in a transaction since lost.
		`UPDATE sqlite_sequence SET seq = 8 WHERE name = 'time_entries'`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportCopiesEveryRowAsItWasAndNumbersOnAfterIt(t *testing.T) {
	store := testStore(t)
	counts, err := ImportStandaloneDatabase(testContext, store.db, newStandaloneDatabase(t), aliceID)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{TimeEntries: 3, Revisions: 1, Kinds: 2, KindAssignments: 2, Reviews: 1, Links: 4}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	if got := counts.Summary(); got != "time_entries=3 revisions=1 kinds=2 kind_assignments=2 reviews=1 links=4" {
		t.Errorf("summary = %q", got)
	}

	workshop, err := store.Entry(testContext, aliceScope, "time_entry_000002", true)
	if err != nil {
		t.Fatal(err)
	}
	if workshop.OwnerPrincipalID != aliceID || workshop.ParentTimeEntryID != "time_entry_000005" || workshop.Place != "Hall" || workshop.UpdatedAt != 200 ||
		len(workshop.Links) != 1 || workshop.Links[0].Ref != "calendar_event_000012" || *workshop.Links[0].OccurrenceStartsAt != 2000 || workshop.Outcome != OutcomeUnknown {
		t.Fatalf("time_entry_000002 = %+v", workshop)
	}
	away, err := store.Entry(testContext, aliceScope, "time_entry_000005", true)
	if err != nil {
		t.Fatal(err)
	}
	if !away.AllDay || away.StartDate != "2024-03-01" || len(away.ChildIDs) != 1 || away.ChildIDs[0] != "time_entry_000002" ||
		len(away.Kinds) != 1 || away.Kinds[0].KindID != "time_entry_kind_000001" || len(away.Links) != 2 {
		t.Fatalf("time_entry_000005 = %+v", away)
	}
	for _, link := range away.Links {
		switch link.LinkType {
		case "trips":
			if *link.OverlapSeconds != 8000 || link.RemovedAt != nil {
				t.Errorf("trip link = %+v", link)
			}
		case "transactions":
			if *link.AmountCents != 2350 || link.RemovedAt == nil || *link.RemovedAt != 170 {
				t.Errorf("transaction link = %+v", link)
			}
		default:
			t.Errorf("unexpected link %+v", link)
		}
	}
	skipped, err := store.Entry(testContext, aliceScope, "time_entry_000006", true)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.DeletedAt == nil || *skipped.DeletedAt != 400 || skipped.Outcome != OutcomeSkipped || skipped.OutcomeReviewID != "time_entry_review_000009" ||
		len(skipped.Kinds) != 0 || len(skipped.Reviews) != 1 {
		t.Fatalf("time_entry_000006 = %+v", skipped)
	}
	review := skipped.Reviews[0]
	if review.CostUSD != 0.0375 || string(review.LinksSeen) != `[{"link_type":"visits"}]` || review.SuggestedKindNames[0] != "class" ||
		review.SuggestedParentTimeEntryID != "time_entry_000005" || review.InputTokens != 1200 {
		t.Fatalf("review = %+v", review)
	}
	revisions, err := store.Revisions(testContext, aliceScope, "time_entry_000005")
	if err != nil || len(revisions) != 1 || revisions[0]["revision_id"] != int64(3) || string(revisions[0]["entry"].(json.RawMessage)) != `{"id":"time_entry_000005","title":"Away"}` {
		t.Fatalf("revisions = %+v %v", revisions, err)
	}
	if _, err := store.Entry(testContext, bobScope, "time_entry_000005", false); err == nil {
		t.Error("another owner reads an imported entry")
	}

	// The owner already holds kinds, so none are seeded over the imported ones.
	kinds, err := store.Kinds(testContext, aliceScope)
	if err != nil || len(kinds) != 2 {
		t.Fatalf("kinds after the import = %+v %v", kinds, err)
	}
	// The next records take the numbers after the highest ever handed out.
	next, err := store.CreateEntry(testContext, aliceID, entryInput("Dinner", 6000, 7000, operator))
	if err != nil || next.ID != "time_entry_000009" {
		t.Fatalf("next entry = %+v %v", next, err)
	}
	kind, err := store.CreateKind(testContext, aliceID, "dinner", "a meal out", operator)
	if err != nil || kind.ID != "time_entry_kind_000005" {
		t.Fatalf("next kind = %+v %v", kind, err)
	}
	nextReview, err := store.AddReview(testContext, aliceScope, next.ID, ReviewInput{Outcome: OutcomeDone, Reason: "r", By: operator.By, ByKind: operator.Kind})
	if err != nil || nextReview.ID != "time_entry_review_000010" {
		t.Fatalf("next review = %+v %v", nextReview, err)
	}
	title := "Long dinner"
	if _, err := store.UpdateEntry(testContext, aliceScope, next.ID, EntryChange{Title: &title, By: operator.By, ByKind: operator.Kind, Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	revisions, err = store.Revisions(testContext, aliceScope, next.ID)
	if err != nil || len(revisions) != 1 || revisions[0]["revision_id"] != int64(4) {
		t.Fatalf("next revision = %+v %v", revisions, err)
	}
	// An imported source keeps its entry: the importer finds it and does not
	// make it again.
	if _, err := store.CreateEntry(testContext, aliceID, entryInput("Workshop", 2000, 3000, importer, calendarSource("calendar_event_000012", 2000))); err == nil ||
		!strings.Contains(err.Error(), "time_entry_000002") {
		t.Fatalf("a second entry for an imported source: %v", err)
	}
}

func TestImportRefusesWhatItCannotCopyWhole(t *testing.T) {
	store := testStore(t)
	path := newStandaloneDatabase(t)
	for _, owner := range []string{"", "alice", "principal_1"} {
		if _, err := ImportStandaloneDatabase(testContext, store.db, path, owner); err == nil {
			t.Errorf("owner %q was taken", owner)
		}
	}
	if _, err := ImportStandaloneDatabase(testContext, store.db, filepath.Join(t.TempDir(), "missing.db"), aliceID); err == nil {
		t.Error("a missing file was imported as an empty one")
	}
	if _, err := ImportStandaloneDatabase(testContext, store.db, path, aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandaloneDatabase(testContext, store.db, path, aliceID); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second import: %v, want a refusal", err)
	}

	other := testStore(t)
	alter := func(statement string) string {
		t.Helper()
		changed := newStandaloneDatabase(t)
		db, err := sql.Open("sqlite", changed)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if _, err := ImportStandaloneDatabase(testContext, other.db, alter(`ALTER TABLE time_entry_photos ADD COLUMN caption TEXT NOT NULL DEFAULT ''`), aliceID); err == nil ||
		!strings.Contains(err.Error(), "caption") {
		t.Errorf("a column the import does not know: %v", err)
	}
	if _, err := ImportStandaloneDatabase(testContext, other.db, alter(`UPDATE time_entry_kinds SET id = 'time_entry_kind_000009' WHERE seq = 4`), aliceID); err == nil {
		t.Error("an id its number does not make was copied")
	}
	if _, err := ImportStandaloneDatabase(testContext, other.db, alter(`UPDATE time_entries SET parent_time_entry_id = 'time_entry_000099' WHERE seq = 2`), aliceID); err == nil {
		t.Error("an entry inside one the file does not hold was copied")
	}
	if counts, err := other.Counts(testContext, ownership.EveryOwner()); err != nil || counts["time_entries"] != 0 || counts["kinds"] != 0 || counts["links"] != 0 {
		t.Errorf("a refused import left rows: %+v %v", counts, err)
	}
}
