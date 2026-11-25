package operationstore_test

import (
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
)

func openBenchmarkStore(b *testing.B) *operationstore.Store {
	store, err := operationstore.Open(filepath.Join(b.TempDir(), "operations.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { store.Close() })
	return store
}
