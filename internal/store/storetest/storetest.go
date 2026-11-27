// Package storetest gives a test the bridge's own records in a Postgres
// database of its own: the bridge core module and the session log module,
// migrated, with the session log let to write the rows of its events, as a
// server sets them up at start.
//
// The Postgres is the one postgrestest names (POSTGRESTEST_ADMINISTRATOR_URL;
// scripts/test-postgres.sh starts one).
package storetest

import (
	"context"
	"database/sql"
	"testing"

	sessionlogserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/server"
	sessionlogstore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/store"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// Database is one test's database holding both modules.
type Database struct {
	*postgrestest.Database
	t                testing.TB
	coreModule       postgresstorage.Module
	sessionLogModule postgresstorage.Module
}

// NewDatabase makes the test's database. It is dropped when the test ends.
func NewDatabase(t testing.TB) *Database {
	t.Helper()
	coreModule, err := store.Module()
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	sessionLogModule, err := sessionlogstore.Module()
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	return &Database{Database: postgrestest.NewDatabase(t, sessionLogModule, coreModule), t: t, coreModule: coreModule, sessionLogModule: sessionLogModule}
}

// OpenStore opens the bridge core store on the database, as a server opens it
// at start, and lets the session log write the rows of its events; a second
// call opens it again over the same rows. It is closed when the test ends.
func (database *Database) OpenStore() *store.Store {
	database.t.Helper()
	writer := database.Database.Open(database.coreModule, postgresstorage.ScanTimestampsInUTC)
	reader, err := store.OpenReader(context.Background(), database.URL(), database.coreModule)
	if err != nil {
		database.t.Fatalf("storetest: %v", err)
	}
	database.t.Cleanup(func() { reader.Close() })
	opened, err := store.New(writer, reader)
	if err != nil {
		database.t.Fatalf("storetest: %v", err)
	}
	if err := opened.GrantSessionLogWrites(context.Background(), database.sessionLogModule.Role); err != nil {
		database.t.Fatalf("storetest: %v", err)
	}
	return opened
}

// OpenSessionLogPools opens the session log module's writer and reader pools
// on the database, as a server opens them. They are closed when the test ends.
func (database *Database) OpenSessionLogPools(asynchronousCommit bool) (writer, reader *sql.DB) {
	database.t.Helper()
	writerURL, err := sessionlogstore.WriterURL(database.URL(), asynchronousCommit)
	if err != nil {
		database.t.Fatalf("storetest: %v", err)
	}
	writer, err = postgresstorage.Open(context.Background(), writerURL, database.sessionLogModule)
	if err != nil {
		database.t.Fatalf("storetest: %v", err)
	}
	database.t.Cleanup(func() { writer.Close() })
	reader, err = sessionlogstore.OpenReader(context.Background(), database.URL(), database.sessionLogModule)
	if err != nil {
		database.t.Fatalf("storetest: %v", err)
	}
	database.t.Cleanup(func() { reader.Close() })
	return writer, reader
}

// OpenSessionLog opens the session log module on the database as the server
// runs it, with no logstack forwarder.
func (database *Database) OpenSessionLog(asynchronousCommit bool) *sessionlogserver.Server {
	database.t.Helper()
	writer, reader := database.OpenSessionLogPools(asynchronousCommit)
	return sessionlogserver.New(sessionlogstore.New(writer, reader), nil)
}

// RecordArchivedEventNumbering records highest as the highest id the archived
// SQLite events table handed out, as the import of a bridge database does.
func (database *Database) RecordArchivedEventNumbering(highest int64) {
	database.t.Helper()
	core := database.Database.Open(database.coreModule)
	if _, err := core.Exec(`INSERT INTO archived_event_numbering (only_row, highest_archived_event_id, recorded_at, source_description) VALUES (1, $1, now(), 'a test')`, highest); err != nil {
		database.t.Fatalf("storetest: %v", err)
	}
}

// CoreModule is the bridge core module the database holds.
func (database *Database) CoreModule() postgresstorage.Module { return database.coreModule }

// New is a fresh bridge core store in a database of the test's own.
func New(t testing.TB) *store.Store {
	t.Helper()
	return NewDatabase(t).OpenStore()
}
