// Package harnessstoretest gives a test a harness-store of its own, in a
// Postgres database made for that test (postgrestest), which needs
// POSTGRESTEST_ADMINISTRATOR_URL.
package harnessstoretest

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/machinemanagement/harnessstore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// NewStore answers an empty store on a database dropped when the test ends.
func NewStore(t testing.TB) *harnessstore.Store {
	t.Helper()
	module, err := harnessstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	return harnessstore.New(database.Open(module))
}
