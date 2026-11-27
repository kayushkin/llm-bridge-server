// Package messages keeps every chat message the user sent or received,
// one row each in time order, whatever app carried it: texts, Skype,
// Messenger, Discord, and what multichat logs live.
//
// Conversations, messages and conversation days are cited elsewhere by their
// ids (conversation_000001, chat_message_000001, conversation_day_000001),
// which reference-store lists, so a timeline can link one as a chip.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New. Every record has an owner (owner_principal_id), and
// every read and write is narrowed to an ownership.Scope.
package messages

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
)

// The type parts of the public ids. Each record type is listed in
// reference-store under the same name.
const (
	ConversationIDPrefix    = "conversation"
	MessageIDPrefix         = "chat_message"
	ConversationDayIDPrefix = "conversation_day"
	ImportIDPrefix          = "message_import"
	AccountIDPrefix         = "message_account"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "messages", Role: "messages_module", Migrations: migrations}, nil
}

// Store keeps conversations, messages, days, imports and accounts in the
// module's tables.
type Store struct {
	db *sql.DB
	// writeMutex serializes every write, as the standalone store did: two
	// batches that file the same new conversation never race to make it.
	writeMutex sync.Mutex
}

// New is the store over db, the module's pool (Module).
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// Ping checks the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func nowMillis() int64 { return time.Now().UnixMilli() }

// queryer is a pool or a transaction.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// arguments collects a query's arguments and hands out their placeholders.
type arguments struct{ values []any }

func (a *arguments) add(value any) string {
	a.values = append(a.values, value)
	return "$" + strconv.Itoa(len(a.values))
}

// scope adds the scope as a condition on column. A scope that was never set
// is an error, not "everyone".
func (a *arguments) scope(scope ownership.Scope, column string) (string, error) {
	if err := scope.Check(); err != nil {
		return "", err
	}
	if scope.IsEveryOwner() {
		return "TRUE", nil
	}
	return column + " = " + a.add(scope.OwnerPrincipalID()), nil
}

// recordNumber reads the number of an id of one type. A malformed id names
// nothing, so it is reported as not found, as a missing one is.
func recordNumber(prefix, kind, id string) (int64, error) {
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil {
		return 0, fmt.Errorf("%w: %s %s", ErrNotFound, kind, id)
	}
	return number, nil
}

func formatOptionalImport(seq sql.NullInt64) string {
	if !seq.Valid {
		return ""
	}
	return postgresstorage.FormatRecordID(ImportIDPrefix, seq.Int64)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
