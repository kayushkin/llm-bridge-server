package skills

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testContext is the context every store call in these tests runs under.
var testContext = context.Background()

// newTestStore gives the test a store on a Postgres database of its own (made
// from a template with the module migrated, postgresstorage/postgrestest), with
// a data directory and an install directory in a temp dir. The install dir is
// NOT a parent or child of anything the tests use as a source root.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return newTestStoreIn(t, filepath.Join(dir, "data"), filepath.Join(dir, "install"))
}

// newTestStoreIn is newTestStore with the two directories named by the test.
func newTestStoreIn(t *testing.T, dataDirectory, installDirectory string) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	s, err := New(database.Open(module), dataDirectory, installDirectory)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

// withInstallDir is the same records and data directory seen by a store whose
// install directory is installDirectory: what a server restarted with the
// setting changed would open.
func withInstallDir(t *testing.T, s *Store, installDirectory string) *Store {
	t.Helper()
	moved, err := New(s.DB(), s.DataDir(), installDirectory)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return moved
}

// registerTestRoutes mounts the handlers at the paths the standalone service
// served, as the server mounts them behind its prefix.
func registerTestRoutes(mux *http.ServeMux, s *Store) {
	h := NewHandlers(s)
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /sources", h.ListSources)
	mux.HandleFunc("POST /sources", h.CreateSource)
	mux.HandleFunc("GET /sources/{id}", h.GetSource)
	mux.HandleFunc("DELETE /sources/{id}", h.DeleteSource)
	mux.HandleFunc("POST /sources/{id}/sync", h.SyncSource)
	mux.HandleFunc("GET /skills", h.ListSkills)
	mux.HandleFunc("GET /skills/{id}", h.GetSkill)
	mux.HandleFunc("DELETE /skills/{id}", h.DeleteSkill)
	mux.HandleFunc("GET /skills/{id}/files", h.ListSkillFiles)
	mux.HandleFunc("GET /skills/{id}/files/{path...}", h.GetSkillFile)
	mux.HandleFunc("POST /skills/{id}/install", h.InstallSkill)
	mux.HandleFunc("POST /skills/{id}/uninstall", h.UninstallSkill)
	mux.HandleFunc("GET /installations", h.ListInstallations)
	mux.HandleFunc("GET /seed/profile", h.GetSeedProfile)
	mux.HandleFunc("PUT /seed/profile", h.PutSeedProfile)
	mux.HandleFunc("GET /seed/manifest", h.GetSeedManifest)
	mux.HandleFunc("GET /versions/{vid}/content", h.GetVersionContent)
	mux.HandleFunc("POST /seed/observe", h.PostSeedObserve)
	mux.HandleFunc("POST /seed/drift", h.PostSeedDrift)
}
