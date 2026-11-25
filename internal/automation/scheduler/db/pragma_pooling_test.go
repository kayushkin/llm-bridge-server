package db

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
func readPragmaFromEveryPooledConnection(t *testing.T, database *sql.DB, pragma string) []int {
	t.Helper()
	ctx := context.Background()
	database.SetMaxOpenConns(connectionsHeldOpen)

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
		connection, err := database.Conn(ctx)
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

// TestTheDSNCarriesBusyTimeoutToEveryConnection asks the DSN directly rather
// than going through New, because New caps the store at one connection. The
// point is that the setting travels with the connection string, so it survives
// that cap being raised or the single connection being replaced.
func TestTheDSNCarriesBusyTimeoutToEveryConnection(t *testing.T) {
	database, err := sql.Open("sqlite", dataSourceName(filepath.Join(t.TempDir(), "scheduler.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer database.Close()

	for connectionIndex, value := range readPragmaFromEveryPooledConnection(t, database, "busy_timeout") {
		if value != busyTimeoutMillisecondsWanted {
			t.Errorf("pooled connection %d reports busy_timeout %d, want %d",
				connectionIndex, value, busyTimeoutMillisecondsWanted)
		}
	}
}

// TestNewOpensWithBusyTimeoutSet covers the store the rest of the scheduler
// actually uses.
func TestNewOpensWithBusyTimeoutSet(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	var busyTimeoutMilliseconds int
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeoutMilliseconds); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeoutMilliseconds != busyTimeoutMillisecondsWanted {
		t.Errorf("store reports busy_timeout %d, want %d",
			busyTimeoutMilliseconds, busyTimeoutMillisecondsWanted)
	}
}

// TestTheOneShotPragmaMissesMostOfAPool is the control, and it is the reason
// the tests above mean anything. It reproduces the form the scheduler used to
// be configured with and asserts the instrument can still report the broken
// state. Without it, a reader cannot tell "every connection is configured"
// from "the check silently stopped looking".
func TestTheOneShotPragmaMissesMostOfAPool(t *testing.T) {
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		t.Fatalf("one-shot pragmas: %v", err)
	}

	values := readPragmaFromEveryPooledConnection(t, database, "busy_timeout")
	connectionsMissingTheSetting := 0
	for _, value := range values {
		if value != busyTimeoutMillisecondsWanted {
			connectionsMissingTheSetting++
		}
	}
	if connectionsMissingTheSetting == 0 {
		t.Fatalf("control did not reproduce the bug: all %d pooled connections report "+
			"busy_timeout %d from a one-shot db.Exec, so this instrument cannot "+
			"distinguish a configured pool from an unconfigured one",
			len(values), busyTimeoutMillisecondsWanted)
	}
	t.Logf("control reproduced the bug: %d of %d pooled connections never saw the one-shot pragma",
		connectionsMissingTheSetting, len(values))
}
