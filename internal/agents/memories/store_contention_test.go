package memories

import (
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// Several stores may use one database at once, some writing while others
// read. Under SQLite that took WAL and a busy timeout, without which a
// contended statement failed at once — and BuildContext's read is the one that
// assembles an agent's identity and instructions, so losing it costs a turn its
// whole system prompt. Postgres takes the concurrency itself; this keeps the
// four stores, forty writes and forty reads that measured it, each store on a
// pool of its own.
func TestConcurrentStoresDoNotFailOnContention(t *testing.T) {
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	stores := make([]*Store, 4)
	for connection := range stores {
		stores[connection] = New(database.Open(module))
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var writeFailures, readFailures []error

	for connection := 0; connection < 4; connection++ {
		wg.Add(1)
		go func(connection int) {
			defer wg.Done()
			store := stores[connection]

			for i := 0; i < 10; i++ {
				err := store.Save(Memory{
					ID:           string(rune('a'+connection)) + string(rune('0'+i%10)),
					Content:      "a memory worth keeping",
					Importance:   0.9,
					Source:       "test",
					LastAccessed: time.Now(),
					CreatedAt:    time.Now(),
				})
				if err != nil {
					mu.Lock()
					writeFailures = append(writeFailures, err)
					mu.Unlock()
				}

				if _, _, err := store.BuildContext(BuildContextRequest{TokenBudget: 4000}); err != nil {
					mu.Lock()
					readFailures = append(readFailures, err)
					mu.Unlock()
				}
			}
		}(connection)
	}
	wg.Wait()

	if len(readFailures) > 0 {
		t.Errorf("%d of 40 BuildContext reads failed under contention, first: %v", len(readFailures), readFailures[0])
	}
	if len(writeFailures) > 0 {
		t.Errorf("%d of 40 writes failed under contention, first: %v", len(writeFailures), writeFailures[0])
	}
}
