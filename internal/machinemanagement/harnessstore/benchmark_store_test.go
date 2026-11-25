package harnessstore

import (
	"path/filepath"
	"testing"
)

func openBenchmarkStore(b *testing.B) *Store {
	store, err := Open(filepath.Join(b.TempDir(), "harness.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { store.Close() })
	return store
}
