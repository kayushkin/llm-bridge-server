package httpapi

import (
	"database/sql"
	"testing"

	usagestore "github.com/kayushkin/llm-bridge-server/internal/agents/usagestore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// newTestUsageStoreAndPool opens an empty store on a Postgres database of the
// test's own, and hands back its pool so a test can close it under the store.
func newTestUsageStoreAndPool(t *testing.T) (*usagestore.Store, *sql.DB) {
	t.Helper()
	module, err := usagestore.Module()
	if err != nil {
		t.Fatalf("usage-store module: %v", err)
	}
	pool := postgrestest.NewDatabase(t, module).Open(module)
	return usagestore.New(pool), pool
}

func newTestUsageStore(t *testing.T) *usagestore.Store {
	t.Helper()
	store, _ := newTestUsageStoreAndPool(t)
	return store
}
