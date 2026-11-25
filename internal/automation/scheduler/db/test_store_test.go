package db

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testStore answers an empty store on a Postgres database of the test's own.
func testStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	return New(postgrestest.NewDatabase(t, module).Open(module))
}
