// Package messagestore keeps every chat message the user sent or received,
// one row each in time order, whatever app carried it: texts, Skype,
// Messenger, Discord, and what multichat logs live.
//
// Conversations, messages and conversation days are cited elsewhere by their
// ids (conversation_000001, chat_message_000001, conversation_day_000001),
// which kanban-store's registry claims, so a timeline can link one as a chip.
package messagestore

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
)

// Id prefixes. Each is claimed by a kanban-store registry type of the same
// name; none contains another's pattern at a word boundary.
const (
	prefixConversation    = "conversation_"
	prefixMessage         = "chat_message_"
	prefixConversationDay = "conversation_day_"
	prefixImport          = "message_import_"
	prefixAccount         = "message_account_"
)

// Store owns the database.
type Store struct {
	db      *sql.DB
	dataDir string
	// writeMutex serializes every write; the next seq is MAX(seq)+1 read
	// inside the write's transaction.
	writeMutex sync.Mutex
}

// Open creates the data directory (0700) if needed, opens the database and
// applies the schema.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("open store: no data directory given")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "message-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if err := addMissingColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(fillBodySources); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: fill body sources: %w", err)
	}
	if err := repairReusedConversationIDs(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: repair reused conversation ids: %w", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dataDir: dataDir}, nil
}

// addedColumns are columns added to a table after it was first made. The
// schema's CREATE TABLE holds them for a new database; an older one gets
// them here, before the schema runs.
var addedColumns = []struct{ table, column, definition string }{
	{"messages", "embeds", "TEXT NOT NULL DEFAULT '[]'"},
	{"messages", "buttons", "TEXT NOT NULL DEFAULT '[]'"},
	{"conversations", "parent_source_ref", "TEXT NOT NULL DEFAULT ''"},
	{"accounts", "credential_id", "TEXT NOT NULL DEFAULT ''"},
	{"messages", "body_source", "TEXT NOT NULL DEFAULT ''"},
}

// fillBodySources names, once, where each older message's text came from:
// the multichat importer's text was the bridge's, any other importer's the
// platform's. A row named bridge wrongly only lets the platform's own text
// replace it on the next read.
const fillBodySources = `UPDATE messages SET body_source = CASE
	WHEN imported_by IN (SELECT id FROM imports WHERE importer = 'message-import-multichat') THEN 'bridge' ELSE 'platform' END
	WHERE body_source = ''`

func addMissingColumns(db *sql.DB) error {
	for _, added := range addedColumns {
		var tableCount int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, added.table).Scan(&tableCount); err != nil {
			return err
		}
		if tableCount == 0 {
			continue
		}
		var columnCount int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, added.table, added.column).Scan(&columnCount); err != nil {
			return err
		}
		if columnCount == 0 {
			if _, err := db.Exec("ALTER TABLE " + added.table + " ADD COLUMN " + added.column + " " + added.definition); err != nil {
				return fmt.Errorf("add %s.%s: %w", added.table, added.column, err)
			}
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is where the database lives.
func (s *Store) DataDir() string { return s.dataDir }

func formatID(prefix string, seq int64) string { return fmt.Sprintf("%s%06d", prefix, seq) }

func nowMillis() int64 { return time.Now().UnixMilli() }

// retiredIDs names, for a table whose rows can be removed, the table that
// keeps each removed row's id and the prefix of that id. A removed id still
// answers (it redirects), so it must never be minted again.
var retiredIDs = map[string]struct{ table, column, prefix string }{
	"conversations": {"conversation_merges", "conversation_id", prefixConversation},
	"messages":      {"superseded_messages", "id", prefixMessage},
}

// nextSeq reads the next seq of table inside tx: one past both the highest
// live row and the highest id retired from it.
func nextSeq(tx *sql.Tx, table string) (int64, error) {
	var max sql.NullInt64
	if err := tx.QueryRow("SELECT MAX(seq) FROM " + table).Scan(&max); err != nil {
		return 0, err
	}
	if retired, ok := retiredIDs[table]; ok {
		var retiredMax sql.NullInt64
		query := fmt.Sprintf("SELECT MAX(CAST(substr(%s, %d) AS INTEGER)) FROM %s", retired.column, len(retired.prefix)+1, retired.table)
		if err := tx.QueryRow(query).Scan(&retiredMax); err != nil {
			return 0, fmt.Errorf("highest id retired from %s: %w", table, err)
		}
		if retiredMax.Int64 > max.Int64 {
			max = retiredMax
		}
	}
	return max.Int64 + 1, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
