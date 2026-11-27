// Package hookstest gives a test a hook-store of its own, in a Postgres
// database made for that test (postgrestest), which needs
// POSTGRESTEST_ADMINISTRATOR_URL.
package hookstest

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/agents/hooks"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// NewStore answers an empty store on a database dropped when the test ends.
func NewStore(t testing.TB) *hooks.Store {
	t.Helper()
	module, err := hooks.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	return hooks.New(database.Open(module))
}
