// Package dbtest gives a test a scheduler store of its own, in a Postgres
// database made for that test (postgrestest), which needs
// POSTGRESTEST_ADMINISTRATOR_URL.
package dbtest

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// NewStore answers an empty store on a database dropped when the test ends.
func NewStore(t testing.TB) *db.Store {
	t.Helper()
	store, _ := NewStoreAndPool(t)
	return store
}

// NewStoreAndPool answers an empty store and the module pool under it, for a
// test that reads or writes the tables directly.
func NewStoreAndPool(t testing.TB) (*db.Store, *postgrestest.Database) {
	t.Helper()
	module, err := db.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	return db.New(database.Open(module)), database
}
