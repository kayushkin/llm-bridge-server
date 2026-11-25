// Package snapshotstoretest gives a test a snapshot-store of its own: rows in
// a Postgres database made for that test (postgrestest), which needs
// POSTGRESTEST_ADMINISTRATOR_URL, and blobs in a git folder under the test's
// temp dir.
package snapshotstoretest

import (
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/snapshotstore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// NewStore answers an empty store on a database dropped when the test ends.
func NewStore(t testing.TB) *snapshotstore.Store {
	t.Helper()
	module, err := snapshotstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	store, err := snapshotstore.New(database.Open(module), snapshotstore.Config{GitDir: filepath.Join(t.TempDir(), "snapshots.git")})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
