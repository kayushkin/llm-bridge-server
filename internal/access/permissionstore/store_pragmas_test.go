package permstore

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// This store spent its whole life on the rollback journal with no busy timeout,
// while its own DSN and its Store doc both said WAL. modernc.org/sqlite accepts
// _pragma=NAME(VALUE) and silently drops any other key, so the mattn-style
// "?_journal_mode=WAL&_busy_timeout=5000" it shipped with applied neither and
// reported nothing — the live database read byte 18 of its header as 1.
//
// Asserting on the pragmas rather than on the DSN string is what makes this
// unfakeable: a misspelled key and a correct one look identical from outside.
func TestOpenAppliesItsConcurrencyPragmas(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "permission-store.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	var settled string
	if err := store.DB().QueryRow("PRAGMA journal_mode").Scan(&settled); err != nil {
		t.Fatalf("read back journal_mode: %v", err)
	}
	if !strings.EqualFold(settled, journalMode) {
		t.Errorf("journal_mode is %q, want %q — the conversion did not reach SQLite", settled, journalMode)
	}

	var busyTimeout int
	if err := store.DB().QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read back busy_timeout: %v", err)
	}
	if busyTimeout != busyTimeoutMilliseconds {
		t.Errorf("busy_timeout is %d, want %d — the DSN key did not reach SQLite", busyTimeout, busyTimeoutMilliseconds)
	}
}

// The conversion has to be a statement Open runs, not a DSN pragma the driver
// replays on every connection, because only the statement can retry: SQLite will
// not run the busy handler for the lock upgrade the conversion needs.
//
// Holding a write transaction on the fresh file makes that deterministic. The 0s
// is the assertion — a store honoring its own 5s busy_timeout would have
// outlasted a 150ms lock.
func TestOpenWaitsOutAConversionItLoses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permission-store.db")

	// The holder's DSN is spelled out rather than reusing dsnParams: it has to
	// leave the fresh file on the rollback journal so the store under test is the
	// one that must convert it.
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
	store, err := Open(path)
	waited := time.Since(opened)
	if err != nil {
		t.Fatalf("Open gave up while another connection held the file, after %v: %v", waited, err)
	}
	defer store.Close()

	if waited < holdFor {
		t.Errorf("Open returned after %v, before the %v lock was released — it cannot have waited for it", waited, holdFor)
	}
}
