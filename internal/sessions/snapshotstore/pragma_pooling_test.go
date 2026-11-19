package snapshotstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// connectionsHeldOpen is deliberately larger than one. The bug this file pins
// is invisible at one connection: a pragma set by a single db.Exec lands on
// whichever connection the pool hands out, and that connection answers
// correctly forever. Only holding several open at once exposes the rest.
const connectionsHeldOpen = 8

// readPragmaFromEveryPooledConnection opens connectionsHeldOpen connections,
// holds them all open at the same time, and returns what each one answers for
// the named pragma. Holding them is load-bearing: released connections are
// reused, so a sequential loop would keep asking the same one.
func readPragmaFromEveryPooledConnection(t *testing.T, db *sql.DB, pragma string) []int {
	t.Helper()
	ctx := context.Background()
	db.SetMaxOpenConns(connectionsHeldOpen)

	// Released only after every value is collected: the connections must be
	// held simultaneously for the read to mean anything, but holding them past
	// the return would exhaust the pool for the next call on the same *sql.DB.
	connections := make([]*sql.Conn, 0, connectionsHeldOpen)
	defer func() {
		for _, connection := range connections {
			connection.Close()
		}
	}()

	values := make([]int, 0, connectionsHeldOpen)
	for i := 0; i < connectionsHeldOpen; i++ {
		connection, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("open pooled connection %d: %v", i, err)
		}
		connections = append(connections, connection)

		var value int
		if err := connection.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&value); err != nil {
			t.Fatalf("read PRAGMA %s on connection %d: %v", pragma, i, err)
		}
		values = append(values, value)
	}
	return values
}

func openStoreForPragmaTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(Config{
		DBPath: filepath.Join(dir, "snapshots.db"),
		GitDir: filepath.Join(dir, "snapshots.git"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestBusyTimeoutIsSetOnEveryPooledConnection(t *testing.T) {
	for connectionIndex, value := range readPragmaFromEveryPooledConnection(t, openStoreForPragmaTest(t).DB(), "busy_timeout") {
		if value != busyTimeoutMillisecondsWanted {
			t.Errorf("pooled connection %d reports busy_timeout %d, want %d",
				connectionIndex, value, busyTimeoutMillisecondsWanted)
		}
	}
}

func TestForeignKeysAreOnEveryPooledConnection(t *testing.T) {
	for connectionIndex, value := range readPragmaFromEveryPooledConnection(t, openStoreForPragmaTest(t).DB(), "foreign_keys") {
		if value != 1 {
			t.Errorf("pooled connection %d reports foreign_keys %d, want 1", connectionIndex, value)
		}
	}
}

// TestPerConnectionPragmaControl is the control for the two tests above, and it
// is the reason their results mean anything. It reproduces the old code — a
// bare sql.Open plus a one-shot db.Exec pragma — and asserts the instrument can
// still report the broken state. Without it, a reader cannot tell "every
// connection is configured" from "the check silently stopped looking".
func TestPerConnectionPragmaControl(t *testing.T) {
	// No DSN, exactly as snapshot-store opened its database before the fix.
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;"); err != nil {
		t.Fatalf("one-shot pragmas: %v", err)
	}

	for _, pragma := range []string{"busy_timeout", "foreign_keys"} {
		values := readPragmaFromEveryPooledConnection(t, db, pragma)
		connectionsMissingTheSetting := 0
		for _, value := range values {
			if value == 0 {
				connectionsMissingTheSetting++
			}
		}
		if connectionsMissingTheSetting == 0 {
			t.Fatalf("control did not reproduce the bug for %s: all %d pooled connections "+
				"were configured by the one-shot pragma, so this instrument cannot "+
				"distinguish a configured pool from an unconfigured one and the sibling "+
				"tests prove nothing", pragma, len(values))
		}
		t.Logf("control reproduced the bug for %s: %d of %d pooled connections never saw the one-shot pragma",
			pragma, connectionsMissingTheSetting, len(values))
	}
}

// TestTheMattnDSNSpellingIsSilentlyIgnored pins why Open verifies the pragmas
// after opening instead of trusting the DSN. modernc accepts an unrecognized
// key without complaint, so a DSN written in the other driver's dialect opens
// cleanly and configures nothing.
func TestTheMattnDSNSpellingIsSilentlyIgnored(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mattn.db")+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open with the mattn spelling returned an error; if this driver "+
			"has started rejecting unknown DSN keys, Open's verification can be "+
			"simplified: %v", err)
	}
	defer db.Close()

	for _, pragma := range []string{"busy_timeout", "foreign_keys"} {
		for connectionIndex, value := range readPragmaFromEveryPooledConnection(t, db, pragma) {
			if value != 0 {
				t.Fatalf("pooled connection %d honored the mattn spelling for %s (got %d); "+
					"this driver now understands it and dataSourceName's comment is stale",
					connectionIndex, pragma, value)
			}
		}
	}
}
