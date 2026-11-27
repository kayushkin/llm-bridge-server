// Package machinestest gives a test a harness-store of its own, in a
// Postgres database made for that test (postgrestest), which needs
// POSTGRESTEST_ADMINISTRATOR_URL.
package machinestest

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// NewStore answers an empty store on a database dropped when the test ends.
func NewStore(t testing.TB) *machines.Store {
	t.Helper()
	module, err := machines.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	return machines.New(database.Open(module))
}
