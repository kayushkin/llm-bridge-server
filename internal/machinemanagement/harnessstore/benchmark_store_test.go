package harnessstore

import "testing"

func openBenchmarkStore(b *testing.B) *Store {
	return openTestStore(b)
}
