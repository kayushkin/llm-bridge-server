package db

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// newTestStore is a store over a Postgres database of the test's own, with
// the module provisioned and migrated.
func newTestStore(t testing.TB) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	return New(postgrestest.NewDatabase(t, module).Open(module))
}
