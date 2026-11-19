package harnessstore

import (
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// newTestStore opens a fresh harness-store backed by a throwaway on-disk
// SQLite file under the test's temp dir. The file is removed automatically
// when the test finishes. An on-disk path (not ":memory:") is used so the
// WAL pragmas in Open behave the same way they do in production.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "harness-store.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// mustCreateMachine inserts a minimal machine and returns its ID. Instances
// carry a NOT NULL machine_id, so most instance/credential tests need a
// machine to hang off of.
func mustCreateMachine(t *testing.T, s *Store, id, name string) string {
	t.Helper()
	m := &msg.Machine{ID: id, Name: name, Transport: msg.TransportLocal}
	if err := s.CreateMachine(m); err != nil {
		t.Fatalf("CreateMachine(%s): %v", id, err)
	}
	return id
}

func TestOpenIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reopen.db")

	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	mustCreateMachine(t, s1, "m_a", "alpha")
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-opening an existing DB must run migrations cleanly and preserve data.
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	got, err := s2.GetMachine("m_a")
	if err != nil {
		t.Fatalf("GetMachine after reopen: %v", err)
	}
	if got.Name != "alpha" {
		t.Errorf("Name = %q, want alpha", got.Name)
	}
}

func TestDBAccessor(t *testing.T) {
	s := newTestStore(t)
	if s.DB() == nil {
		t.Fatal("DB() returned nil")
	}
	if err := s.DB().Ping(); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestSafeID(t *testing.T) {
	cases := map[string]string{
		"linode":      "linode",
		"WSL-Claude":  "wsl_claude",
		"host.name:1": "host_name_1",
		"":            "m",
		"42abc":       "42abc",
	}
	for in, want := range cases {
		if got := safeID(in); got != want {
			t.Errorf("safeID(%q) = %q, want %q", in, got, want)
		}
	}
}
