package machines

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"github.com/kayushkin/llm-bridge/msg"
)

// openTestStore gives the test a store on a Postgres database of its own, made
// from a template with the module migrated (postgresstorage/postgrestest).
func openTestStore(t testing.TB) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	return New(database.Open(module))
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStore(t)
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

func TestDBAccessor(t *testing.T) {
	s := newTestStore(t)
	if s.DB() == nil {
		t.Fatal("DB() returned nil")
	}
	if err := s.DB().Ping(); err != nil {
		t.Errorf("Ping: %v", err)
	}
}
