package store

import "testing"

// openBridgeCoreReadBenchmarkStore is the store BenchmarkBridgeCoreReads
// reads: the bridge core module on a Postgres database of its own.
func openBridgeCoreReadBenchmarkStore(b *testing.B) *Store {
	return newTestStore(b)
}

// settleBridgeCoreReadBenchmarkStore gives the planner the statistics a
// running server's tables have, which autovacuum would gather a minute after
// the rows were written.
func settleBridgeCoreReadBenchmarkStore(b *testing.B, s *Store) {
	if _, err := s.db.Exec(`ANALYZE`); err != nil {
		b.Fatal(err)
	}
}
