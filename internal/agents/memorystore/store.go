package memorystore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// busyTimeout is how long a statement waits for another connection to release
// the database before giving up with SQLITE_BUSY.
const busyTimeout = 5000

// journalMode is what lets a reader and a writer hold the database at the same
// time. It is not a tuning knob here, it is the fix.
//
// One store per session and several sessions per repository means several
// connections use one file at once, and on the default rollback journal that is
// not concurrency, it is a queue: a reader blocks the writer's commit and a
// writer blocks every reader. SQLite's default is also not to wait at all, so a
// statement that arrives at a locked database fails on the spot. Both matter,
// and only together — measured on four concurrent stores doing forty writes and
// forty BuildContext reads:
//
//	default                     4 reads and 18 writes failed, in 0.07s
//	busy_timeout alone          8 writes still failed, and it took 50s
//	WAL + busy_timeout          nothing failed, in 0.12s
//
// The middle row is the trap: waiting alone does not break the queue, it only
// makes the failure slow. Readers never clear long enough for the writer to
// upgrade, so the timeout is spent rather than used.
//
// Reads are the ones that hurt — BuildContext's SELECT is what assembles an
// agent's identity and instructions, and a turn that loses it has no system
// prompt at all.
const journalMode = "WAL"

// switchJournalMode moves the database file into journalMode, waiting out the
// other stores converting the same fresh file.
//
// Switching a rollback-journal database into WAL takes a brief exclusive lock,
// and that one statement is the only part of opening a store that busy_timeout
// cannot mediate. Four connections racing to convert one fresh file, 160 opens
// per row:
//
//	journal_mode alone              120 failed
//	busy_timeout + journal_mode       1 failed, after 2ms
//	busy_timeout(30000) + same        2 failed, after 1ms
//
// The third row is the finding: six times the timeout changes nothing and the
// failure still lands in a millisecond, so the wait is never being entered.
// SQLite declines to run the busy handler when a connection has to upgrade a
// lock it already holds, because waiting there is how two connections deadlock;
// it returns SQLITE_BUSY on the spot instead. A longer timeout has nothing to
// give.
//
// So the wait has to be ours. Retrying converges because the race is only ever
// over the first conversion: once any store has won it the file is in WAL, and
// every later connection reads the mode back instead of changing it — 240 of
// 240 concurrent opens against an already-converted file succeeded, against 6
// retries needed in total to convert 60 fresh ones.
//
// Retrying ordinary reads and writes instead of running WAL was tried here
// before and measured worse than doing nothing — 50s of stalling and still
// failing. This is not that. It runs once per store, on the one statement that
// turns WAL on, and everything after it still relies on WAL rather than on
// waiting.
func switchJournalMode(db *sql.DB) error {
	// The conversion gets the same patience as any other contended statement.
	// busy_timeout is already the answer to "how long is this store willing to
	// wait for a lock" and there is no reason for a second number.
	deadline := time.Now().Add(busyTimeout * time.Millisecond)

	var settled string
	var err error
	for backoff := time.Millisecond; ; backoff += time.Millisecond {
		err = db.QueryRow("PRAGMA journal_mode(" + journalMode + ")").Scan(&settled)
		if err == nil && strings.EqualFold(settled, journalMode) {
			return nil
		}
		if !time.Now().Add(backoff).Before(deadline) {
			break
		}
		time.Sleep(backoff)
	}

	if err != nil {
		return fmt.Errorf("switch journal mode to %s: %w", journalMode, err)
	}
	// A lost race can also come back quietly, reporting the mode it stayed on
	// rather than an error, and a store left on the rollback journal is the
	// serialized queue this whole file exists to avoid.
	return fmt.Errorf("journal mode settled on %q, want %s", settled, journalMode)
}

// NewStore creates or opens a memory store at the given path.
func NewStore(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	// modernc's driver ignores a DSN key it does not recognize rather than
	// rejecting it, so `_pragma=` is the only spelling that reaches SQLite and a
	// typo here would leave the setting at its default without saying so.
	//
	// journal_mode is deliberately not in here with it. The driver replays every
	// DSN pragma on every connection the pool opens, so putting the conversion
	// there runs it on connections that cannot retry it and reports the failure
	// as whatever statement happened to open the connection — which is why this
	// arrived as "create schema: database is locked". It belongs below, once,
	// where it can wait.
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)", dbPath, busyTimeout)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := switchJournalMode(db); err != nil {
		db.Close()
		return nil, err
	}

	// Create normalized schema
	// Note: We only create the table and basic indexes here
	// Migrations will add new columns to existing tables
	schema := `
	CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		summary TEXT,
		original_id TEXT,
		importance REAL NOT NULL DEFAULT 0.5,
		access_count INTEGER NOT NULL DEFAULT 0,
		last_accessed INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		source TEXT NOT NULL,
		embedding BLOB
	);
	CREATE INDEX IF NOT EXISTS idx_importance ON memories(importance);
	CREATE INDEX IF NOT EXISTS idx_last_accessed ON memories(last_accessed);
	CREATE INDEX IF NOT EXISTS idx_created_at ON memories(created_at);
	CREATE INDEX IF NOT EXISTS idx_source ON memories(source);

	CREATE TABLE IF NOT EXISTS memory_tags (
		memory_id TEXT NOT NULL,
		tag TEXT NOT NULL,
		FOREIGN KEY (memory_id) REFERENCES memories(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_memory_tags_memory_id ON memory_tags(memory_id);
	CREATE INDEX IF NOT EXISTS idx_memory_tags_tag ON memory_tags(tag);

	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	// Create session-related tables
	if _, err := db.Exec(SessionsSchema()); err != nil {
		db.Close()
		return nil, fmt.Errorf("create sessions schema: %w", err)
	}

	// The full-text index Search ranks on. It is created here rather than in
	// runMigrations because it is part of the schema a new store needs, not a
	// repair to an old one; the backfill below is the repair half.
	if _, err := db.Exec(ftsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create fts schema: %w", err)
	}

	// Run migrations for existing databases
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	// Index whatever the index is missing. A store written before Search moved
	// off cosine similarity has every memory to index; one written after has
	// none, and the statement costs a single scan.
	if err := backfillFullTextIndex(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{
		db:       db,
		embedder: NewEmbedder(),
	}, nil
}

// OpenOrCreate opens an existing memory store or creates a new one.
func OpenOrCreate(rootDir string) (MemoryStore, error) {
	localDBPath := DefaultMemoryPath(rootDir)
	if err := os.MkdirAll(filepath.Dir(localDBPath), 0755); err != nil {
		return nil, fmt.Errorf("create memory directory: %w", err)
	}

	return NewStore(localDBPath)
}

// NewSQLiteStore creates a new SQLite-backed memory store at the given path.
// This is the same as NewStore but with a more descriptive name.
func NewSQLiteStore(dbPath string) (MemoryStore, error) {
	return NewStore(dbPath)
}