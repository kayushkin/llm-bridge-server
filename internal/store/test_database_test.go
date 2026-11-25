package store

import (
	"context"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testDatabase is one test's Postgres database holding this module. The
// package's own tests cannot use storetest, which imports this package.
type testDatabase struct {
	*postgrestest.Database
	t      testing.TB
	module postgresstorage.Module
}

func newTestDatabase(t testing.TB) *testDatabase {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	return &testDatabase{Database: postgrestest.NewDatabase(t, module), t: t, module: module}
}

// open opens the store on the database, as a server does at start; a second
// call opens it again over the same rows.
func (database *testDatabase) open() *Store {
	database.t.Helper()
	writer := database.Database.Open(database.module, postgresstorage.ScanTimestampsInUTC)
	reader, err := OpenReader(context.Background(), database.URL(), database.module)
	if err != nil {
		database.t.Fatal(err)
	}
	database.t.Cleanup(func() { reader.Close() })
	opened, err := New(writer, reader)
	if err != nil {
		database.t.Fatal(err)
	}
	return opened
}

// newTestStore is a fresh store in a database of the test's own.
func newTestStore(t testing.TB) *Store {
	t.Helper()
	return newTestDatabase(t).open()
}
