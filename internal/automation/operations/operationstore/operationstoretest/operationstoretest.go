// Package operationstoretest gives a test an operations store of its own, in
// a Postgres database made for that test (postgrestest), which needs
// POSTGRESTEST_ADMINISTRATOR_URL.
package operationstoretest

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationstore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// NewStore answers an empty store on a database dropped when the test ends.
func NewStore(t testing.TB) *operationstore.Store {
	t.Helper()
	store, _ := NewStoreAndDatabase(t)
	return store
}

// NewStoreAndDatabase answers an empty store and the test database under it,
// for a test that reads the tables directly or opens a second store on them.
func NewStoreAndDatabase(t testing.TB) (*operationstore.Store, *postgrestest.Database) {
	t.Helper()
	module, err := operationstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	return operationstore.New(database.Open(module)), database
}
