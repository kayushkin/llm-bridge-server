package store

import (
	"testing"

	serverids "github.com/kayushkin/llm-bridge-server/internal/ids"
)

func TestServerIDIsMadeOnceAndKeptAcrossRestarts(t *testing.T) {
	database := newTestDatabase(t)
	first := database.open()
	made := first.ThisServerID()
	if !serverids.IsServerID(made) {
		t.Fatalf("ThisServerID() = %q, not a server id", made)
	}
	first.Close()

	reopened := database.open()
	defer reopened.Close()
	if reopened.ThisServerID() != made {
		t.Fatalf("after a restart ThisServerID() = %q, want the id made at first start, %q", reopened.ThisServerID(), made)
	}

	other := newTestStore(t)
	defer other.Close()
	if other.ThisServerID() == made {
		t.Fatalf("two databases made the same server id %q", made)
	}
}
