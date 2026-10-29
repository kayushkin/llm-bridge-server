package store

import (
	"path/filepath"
	"testing"

	serverids "github.com/kayushkin/llm-bridge-server/internal/ids"
)

func TestServerIDIsMadeOnceAndKeptAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	first, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	made := first.ThisServerID()
	if !serverids.IsServerID(made) {
		t.Fatalf("ThisServerID() = %q, not a server id", made)
	}
	first.Close()

	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.ThisServerID() != made {
		t.Fatalf("after a restart ThisServerID() = %q, want the id made at first start, %q", reopened.ThisServerID(), made)
	}

	other, err := New(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.ThisServerID() == made {
		t.Fatalf("two databases made the same server id %q", made)
	}
}
