package memories

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// openTestStore gives the test a store on a Postgres database of its own, made
// from a template with the module migrated (postgresstorage/postgrestest).
func openTestStore(t testing.TB) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	return New(database.Open(module))
}
