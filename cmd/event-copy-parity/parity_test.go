package main

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

func TestCompareSessionCopiesMatchesByBodyAndCountsRepeats(t *testing.T) {
	bridge := []storedEvent{
		{RowID: 1, Type: "user_message", Data: `{"a":1}`},
		{RowID: 2, Type: "block", Data: `{"b":2}`},
		{RowID: 3, Type: "block", Data: `{"b":2}`},
		{RowID: 4, Type: "system", Data: `{"system":{"subtype":"harness_id_set"}}`},
	}
	// Other row ids and timestamps, the repeated block only once, and one body
	// the bridge does not have.
	logStore := []storedEvent{
		{RowID: 90, Type: "block", Data: `{"b":2}`, CreatedAt: "later"},
		{RowID: 91, Type: "user_message", Data: `{"a":1}`, CreatedAt: "later"},
		{RowID: 92, Type: "session_status", Data: `{"status":{"state":"idle"}}`},
	}
	comparison := compareSessionCopies("s", bridge, logStore)
	if !comparison.differs() {
		t.Fatal("copies that disagree compared equal")
	}
	var onlyInBridgeRows, onlyInLogStoreRows []int64
	for _, ev := range comparison.OnlyInBridge {
		onlyInBridgeRows = append(onlyInBridgeRows, ev.RowID)
	}
	for _, ev := range comparison.OnlyInLogStore {
		onlyInLogStoreRows = append(onlyInLogStoreRows, ev.RowID)
	}
	if want := []int64{3, 4}; !reflect.DeepEqual(onlyInBridgeRows, want) {
		t.Errorf("only in bridge rows %v, want %v", onlyInBridgeRows, want)
	}
	if want := []int64{92}; !reflect.DeepEqual(onlyInLogStoreRows, want) {
		t.Errorf("only in log-store rows %v, want %v", onlyInLogStoreRows, want)
	}
}

func TestCompareSessionCopiesAgreesWhenOnlyOrderAndIdsDiffer(t *testing.T) {
	bridge := []storedEvent{{RowID: 1, Data: "x"}, {RowID: 2, Data: "y"}}
	logStore := []storedEvent{{RowID: 7, Data: "y"}, {RowID: 8, Data: "x"}}
	if comparison := compareSessionCopies("s", bridge, logStore); comparison.differs() {
		t.Fatalf("equal copies reported as differing: %+v", comparison)
	}
}

func TestEventClassNamesTheSystemSubtype(t *testing.T) {
	cases := map[storedEvent]string{
		{Type: "session_status", Data: `{}`}:                                       "session_status",
		{Type: "system", Data: `{"system":{"subtype":"harness_id_set"}}`}:          "system/harness_id_set",
		{Type: "system", Data: `not json`}:                                         "system/(unparseable)",
		{Type: "system", Data: `{"type":"system","system":{"message":"no kind"}}`}: "system/",
	}
	for ev, want := range cases {
		if got := eventClass(ev); got != want {
			t.Errorf("eventClass(%s %s) = %q, want %q", ev.Type, ev.Data, got, want)
		}
	}
}

// The window query runs against the real schema's CURRENT_TIMESTAMP text, so
// it is tested on a database rather than on a slice.
func TestSessionsQuietInWindowLeavesOutOldAndStillWritingSessions(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
		type TEXT NOT NULL, data TEXT NOT NULL, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	insert := func(sessionID, createdAt string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO events (session_id, type, data, created_at) VALUES (?, 'block', '{}', ?)`, sessionID, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	insert("quiet", "2026-09-24 10:00:00")
	insert("quiet", "2026-09-24 11:00:00")
	insert("started-before-window", "2026-09-20 10:00:00")
	insert("started-before-window", "2026-09-24 09:00:00")
	insert("only-before-window", "2026-09-20 10:00:00")
	insert("still-writing", "2026-09-24 10:00:00")
	insert("still-writing", "2026-09-24 11:55:00")
	insert("still-writing-exactly-at-bound", "2026-09-24 11:50:00")

	got, err := sessionsQuietInWindow(db, "2026-09-24 00:00:00", "2026-09-24 11:50:00")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"quiet", "started-before-window"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sessions %v, want %v", got, want)
	}

	latest, err := latestEventCreatedAt(db, "still-writing")
	if err != nil {
		t.Fatal(err)
	}
	if latest != "2026-09-24 11:55:00" {
		t.Errorf("latest %q, want 2026-09-24 11:55:00", latest)
	}
	if latest, err := latestEventCreatedAt(db, "no-such-session"); err != nil || latest != "" {
		t.Errorf("latest of a missing session = %q, %v; want empty, nil", latest, err)
	}
}
