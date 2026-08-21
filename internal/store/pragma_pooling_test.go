package store

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

// TestReaderPoolBusyTimeoutIsSetOnEveryConnection is the point of this file.
// The reader pool is the one that actually runs several connections at once,
// and it was opened with a DSN key this driver does not recognise.
func TestReaderPoolBusyTimeoutIsSetOnEveryConnection(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	for connectionIndex, value := range readPragmaFromEveryPooledConnection(t, s.dbRO, "busy_timeout") {
		if value != busyTimeoutMillisecondsWanted {
			t.Errorf("reader connection %d reports busy_timeout %d, want %d",
				connectionIndex, value, busyTimeoutMillisecondsWanted)
		}
	}
}

func TestWriterBusyTimeoutIsSet(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// The writer is deliberately capped at one connection, so this asks the
	// only connection there is rather than sweeping a pool.
	var busyTimeoutMilliseconds int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeoutMilliseconds); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeoutMilliseconds != busyTimeoutMillisecondsWanted {
		t.Errorf("writer reports busy_timeout %d, want %d",
			busyTimeoutMilliseconds, busyTimeoutMillisecondsWanted)
	}
}

// TestTheOldReaderDSNConfiguredNothing is the control, and it is the reason the
// test above means anything. It reproduces the exact DSN the reader pool used
// to be opened with and asserts the instrument can still report the broken
// state. Without it, a reader cannot tell "every connection is configured"
// from "the check silently stopped looking".
//
// It doubles as the evidence for dataSourceName's comment: the mattn spelling
// does not error, it just does nothing.
func TestTheOldReaderDSNConfiguredNothing(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "control.db")

	db, err := sql.Open("sqlite", databasePath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open with the mattn spelling returned an error; if this driver "+
			"has started rejecting unknown DSN keys, the verification in New can be "+
			"simplified: %v", err)
	}
	defer db.Close()

	values := readPragmaFromEveryPooledConnection(t, db, "busy_timeout")
	connectionsMissingTheSetting := 0
	for _, value := range values {
		if value != busyTimeoutMillisecondsWanted {
			connectionsMissingTheSetting++
		}
	}
	if connectionsMissingTheSetting != len(values) {
		t.Fatalf("control did not reproduce the bug: %d of %d pooled connections were "+
			"configured by ?_busy_timeout=5000, so this driver now understands the "+
			"mattn spelling and dataSourceName's comment is stale",
			len(values)-connectionsMissingTheSetting, len(values))
	}
	t.Logf("control reproduced the bug: %d of %d pooled connections had busy_timeout 0 "+
		"under the old reader DSN", connectionsMissingTheSetting, len(values))
}

// TestTheOneShotPragmaMissesMostOfAPool is the control for the writer's half of
// the change: it reproduces the db.Exec form and shows it does not reach a pool.
func TestTheOneShotPragmaMissesMostOfAPool(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		t.Fatalf("one-shot pragmas: %v", err)
	}

	values := readPragmaFromEveryPooledConnection(t, db, "busy_timeout")
	connectionsMissingTheSetting := 0
	for _, value := range values {
		if value != busyTimeoutMillisecondsWanted {
			connectionsMissingTheSetting++
		}
	}
	if connectionsMissingTheSetting == 0 {
		t.Fatalf("control did not reproduce the bug: all %d pooled connections report "+
			"busy_timeout %d from a one-shot db.Exec", len(values), busyTimeoutMillisecondsWanted)
	}
	t.Logf("control reproduced the bug: %d of %d pooled connections never saw the one-shot pragma",
		connectionsMissingTheSetting, len(values))
}
