package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// New used to carry the WAL conversion in dsnParams, where the driver ran it
// while opening a connection and no retry was possible. A process opening a
// fresh database while anything else held that file failed to open at all, and
// reported it as whichever statement happened to open the connection — in
// memory-store, where this was first found, that was "create schema: database is
// locked", which sent every reader to the schema for months.
//
// Holding a write transaction on the fresh file makes the race deterministic
// rather than roughly-half-the-time. Against the DSN version this fails on every
// run, and it fails instantly: the 0s is the assertion, because a store that had
// honored its own 5s busy_timeout would have outlasted a 150ms lock.
func TestNewWaitsOutAConversionItLoses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noteboard.db")

	// The holder's DSN is spelled out rather than built from dsnParams: it has to
	// leave the fresh file on the rollback journal so the store under test is the
	// one that must convert it. Sharing the constant would let a change to
	// dsnParams quietly convert the file here instead, and the race this test
	// exists for would stop happening.
	holder, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.Exec("CREATE TABLE IF NOT EXISTS seed (id TEXT)"); err != nil {
		t.Fatal(err)
	}
	held, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.Exec("INSERT INTO seed VALUES ('x')"); err != nil {
		t.Fatal(err)
	}

	const holdFor = 150 * time.Millisecond
	releasing := time.AfterFunc(holdFor, func() { held.Commit() })
	defer releasing.Stop()

	opened := time.Now()
	store, err := New(path)
	waited := time.Since(opened)
	if err != nil {
		t.Fatalf("New gave up while another connection held the file, after %v: %v", waited, err)
	}
	defer store.Close()

	if waited < holdFor {
		t.Errorf("New returned after %v, before the %v lock was released — it cannot have waited for it", waited, holdFor)
	}
}

// Complement, and the reason the test above is not just slow-and-lucky: both
// settings have to actually reach SQLite, and they arrive by different routes now
// — busy_timeout as a DSN pragma, journal_mode as a statement switchJournalMode
// runs once. Each route fails quietly in its own way. modernc drops a DSN key it
// does not recognize instead of rejecting it, so the mattn-style spelling this
// store used to carry applied nothing and said nothing; and a lost journal_mode
// race can report the mode the file stayed on rather than an error. Asserting on
// the pragmas, not on the DSN string, is what makes this unfakeable.
func TestNewAppliesItsConcurrencyPragmas(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "noteboard.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	var settled string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&settled); err != nil {
		t.Fatalf("read back journal_mode: %v", err)
	}
	if !strings.EqualFold(settled, journalMode) {
		t.Errorf("journal_mode is %q, want %q — the conversion did not reach SQLite", settled, journalMode)
	}

	var busyTimeout int
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read back busy_timeout: %v", err)
	}
	if busyTimeout != busyTimeoutMilliseconds {
		t.Errorf("busy_timeout is %d, want %d — the DSN key did not reach SQLite", busyTimeout, busyTimeoutMilliseconds)
	}
}
