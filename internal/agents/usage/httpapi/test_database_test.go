package httpapi

import (
	"database/sql"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/agents/usage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// newTestUsageStoreAndPool opens an empty store on a Postgres database of the
// test's own, and hands back its pool so a test can close it under the store.
func newTestUsageStoreAndPool(t *testing.T) (*usage.Store, *sql.DB) {
	t.Helper()
	module, err := usage.Module()
	if err != nil {
		t.Fatalf("usage-store module: %v", err)
	}
	pool := postgrestest.NewDatabase(t, module).Open(module)
	return usage.New(pool), pool
}

func newTestUsageStore(t *testing.T) *usage.Store {
	t.Helper()
	store, _ := newTestUsageStoreAndPool(t)
	return store
}
