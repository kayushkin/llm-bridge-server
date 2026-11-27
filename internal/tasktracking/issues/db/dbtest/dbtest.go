// Package dbtest gives a test a kanban-store over a Postgres database of its
// own, with the module provisioned and migrated. The Postgres is the one
// postgrestest names (POSTGRESTEST_ADMINISTRATOR_URL).
package dbtest

import (
	"database/sql"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/db"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// NewPool is the module's connection pool on a database of the test's own.
// The pool is closed and the database dropped when the test ends.
func NewPool(t testing.TB) *sql.DB {
	t.Helper()
	module, err := db.Module()
	if err != nil {
		t.Fatal(err)
	}
	return postgrestest.NewDatabase(t, module).Open(module)
}

// NewStore is a store over NewPool.
func NewStore(t testing.TB) *db.Store {
	t.Helper()
	return db.New(NewPool(t))
}
