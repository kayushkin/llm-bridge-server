package memorystore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A repository's agents get one store each and run at the same time, so several
// connections write to one file while others read it. SQLite's default is to
// fail a contended statement immediately rather than wait, which turns ordinary
// concurrency into errors — and BuildContext's read is the one that assembles an
// agent's identity and instructions, so losing it costs a turn its whole system
// prompt.
func TestConcurrentStoresDoNotFailOnContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")

	var wg sync.WaitGroup
	var mu sync.Mutex
	var writeFailures, readFailures []error

	for connection := 0; connection < 4; connection++ {
		wg.Add(1)
		go func(connection int) {
			defer wg.Done()
			store, err := NewStore(path)
			if err != nil {
				t.Errorf("connection %d: open: %v", connection, err)
				return
			}
			defer store.Close()

			for i := 0; i < 10; i++ {
				err := store.Save(Memory{
					ID:           string(rune('a'+connection)) + string(rune('0'+i%10)),
					Content:      "a memory worth keeping",
					Importance:   0.9,
					Source:       "test",
					LastAccessed: time.Now(),
					CreatedAt:    time.Now(),
				})
				if err != nil {
					mu.Lock()
					writeFailures = append(writeFailures, err)
					mu.Unlock()
				}

				if _, _, err := store.BuildContext(BuildContextRequest{TokenBudget: 4000}); err != nil {
					mu.Lock()
					readFailures = append(readFailures, err)
					mu.Unlock()
				}
			}
		}(connection)
	}
	wg.Wait()

	if len(readFailures) > 0 {
		t.Errorf("%d of 40 BuildContext reads failed under contention, first: %v", len(readFailures), readFailures[0])
	}
	if len(writeFailures) > 0 {
		t.Errorf("%d of 40 writes failed under contention, first: %v", len(writeFailures), writeFailures[0])
	}
}

// The test above covers a store that is already open. This one covers opening
// it, which turned out to be where the contention actually landed: the test
// above spent months failing roughly half its runs at `create schema: database
// is locked`, before it had performed a single read or write.
//
// The statement that fails is not the schema. It is `PRAGMA journal_mode(WAL)`,
// which needs a brief exclusive lock, and it used to ride in on the DSN — so it
// ran as part of opening the connection, and whichever statement opened that
// connection wore the error. Making it wait is the whole fix, and the wait had
// to be ours: SQLite will not run the busy handler for a lock it has to upgrade,
// so busy_timeout is not entered at all here. That is what this test pins.
//
// Holding a write transaction on the fresh file makes the race deterministic
// rather than roughly-half-the-time. Against the DSN version this fails on every
// run, and it fails instantly — the 0s is the finding, because a store that had
// honored its own 5s busy_timeout would have outlasted a 150ms lock.
func TestNewStoreWaitsOutAConversionItLoses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")

	// Stand in for the store that reached the fresh file first and is mid-write.
	holder, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(%d)", path, busyTimeout))
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
	store, err := NewStore(path)
	waited := time.Since(opened)
	if err != nil {
		t.Fatalf("NewStore gave up while another connection held the file, after %v: %v", waited, err)
	}
	defer store.Close()

	if waited < holdFor {
		t.Errorf("NewStore returned after %v, before the %v lock was released — it cannot have waited for it", waited, holdFor)
	}

	// Succeeding is not enough. A store that came back on the rollback journal
	// has the serialized queue WAL exists to prevent, and says nothing about it.
	var settled string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&settled); err != nil {
		t.Fatalf("read back journal_mode: %v", err)
	}
	if !strings.EqualFold(settled, journalMode) {
		t.Errorf("journal_mode settled on %q, want %q", settled, journalMode)
	}
}

// Complement, and the reason the test above is not just slow-and-lucky: both
// settings have to actually reach SQLite. They arrive by different routes now —
// busy_timeout as a DSN pragma, journal_mode as a statement switchJournalMode
// runs once — and each route fails quietly in its own way. modernc's driver
// drops a DSN key it does not recognize instead of rejecting it, so a misspelled
// one leaves the setting at its default and says nothing; and a lost
// journal_mode race can report the mode it stayed on rather than an error.
func TestStoreAppliesItsConcurrencyPragmas(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var appliedTimeout int
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&appliedTimeout); err != nil {
		t.Fatalf("read back busy_timeout: %v", err)
	}
	if appliedTimeout != busyTimeout {
		t.Errorf("busy_timeout is %d, want %d — the DSN key did not reach SQLite", appliedTimeout, busyTimeout)
	}

	var appliedJournal string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&appliedJournal); err != nil {
		t.Fatalf("read back journal_mode: %v", err)
	}
	if !strings.EqualFold(appliedJournal, journalMode) {
		t.Errorf("journal_mode is %q, want %q — the conversion did not reach SQLite", appliedJournal, journalMode)
	}
}
