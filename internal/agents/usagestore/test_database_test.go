package usagestore

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// newTestStore opens an empty store on a Postgres database of the test's own.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("usage-store module: %v", err)
	}
	return New(postgrestest.NewDatabase(t, module).Open(module))
}
