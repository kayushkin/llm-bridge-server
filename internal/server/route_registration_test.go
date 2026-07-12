package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	agentstore "github.com/kayushkin/agent-store"
	harnessstore "github.com/kayushkin/harness-store"
	hookstore "github.com/kayushkin/hook-store"
	memorystore "github.com/kayushkin/memory-store"
	modelstore "github.com/kayushkin/model-store"
	snapshotstore "github.com/kayushkin/snapshot-store"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// TestRoutesRegisterWithEveryStoreMounted builds the real root mux with every
// embedded store non-nil -- the configuration the production binary actually
// runs -- and asserts that route registration does not panic.
//
// This is the case the rest of the suite structurally cannot reach. Every other
// New() call in these tests passes nil for the embedded stores, and routes()
// mounts a library's handlers only `if s.<store> != nil`. So the gateway's own
// routes get exercised constantly while the mounted libraries' routes are never
// registered under test at all.
//
// That blind spot shipped a real outage-in-waiting on 2026-07-12: agent-store
// began registering "GET /health" from its library entry point, the gateway
// mounts agent-store into its *root* mux and serves its own "GET /health", and
// Go 1.22+ ServeMux panics on a conflicting pattern *at registration time*. The
// result was a committed tree that built cleanly, passed every test, and then
// panicked at boot -- taking llm-bridge.service down on the next deploy. Only a
// real boot could reveal it.
//
// A mounted library can collide with the gateway on any pattern, not just
// /health, and it can do so without either repo changing a line -- go.mod
// `replace`s agent-store with the local working tree, so a sibling repo's
// commit lands in the gateway's next build. Registration is where that
// conflict surfaces, so registration is what this test drives.
func TestRoutesRegisterWithEveryStoreMounted(t *testing.T) {
	srv := newServerWithAllStores(t)

	// The gateway's own /health must be the handler that answers. agent-store
	// (mounted at the root) must not shadow it: healthcheck polls this route
	// and expects the gateway's payload, not a bare {"status":"ok"}.
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET /health: expected 200, got %d", w.Code)
	}
	var health map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if _, ok := health["harnesses"]; !ok {
		t.Errorf("GET /health returned a body with no \"harnesses\" key (%v) -- "+
			"this is the gateway's route, so it must return the gateway's payload. "+
			"A mounted library has shadowed it.", health)
	}

	// ...and the mounted libraries must still be reachable, so that "no panic"
	// can never be achieved by quietly not mounting them.
	for _, route := range []struct{ name, path string }{
		{"agent-store", "/agents"},
		{"memory-store", "/memories/recent"},
	} {
		req := httptest.NewRequest("GET", route.path, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Errorf("GET %s -> 404: %s is not mounted, so this test would not "+
				"catch a route collision from it", route.path, route.name)
		}
	}
}

// newServerWithAllStores constructs a Server with every embedded store opened
// against a temp dir. It deliberately mirrors cmd/llm-bridge-server's wiring:
// if a new store is added there, add it here too, or route collisions from it
// stay invisible to the test suite.
func newServerWithAllStores(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()

	st, err := store.New(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	as, err := agentstore.Open(filepath.Join(dir, "agents.db"))
	if err != nil {
		t.Fatalf("open agent-store: %v", err)
	}
	t.Cleanup(func() { as.Close() })

	ms, err := memorystore.NewStore(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatalf("open memory-store: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	hs, err := harnessstore.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open harness-store: %v", err)
	}
	t.Cleanup(func() { hs.Close() })

	hks, err := hookstore.Open(filepath.Join(dir, "hooks.db"))
	if err != nil {
		t.Fatalf("open hook-store: %v", err)
	}
	t.Cleanup(func() { hks.Close() })

	mds, err := modelstore.Open(filepath.Join(dir, "models.db"))
	if err != nil {
		t.Fatalf("open model-store: %v", err)
	}
	t.Cleanup(func() { mds.Close() })

	ss, err := snapshotstore.Open(snapshotstore.Config{
		DBPath: filepath.Join(dir, "snapshots.db"),
		GitDir: filepath.Join(dir, "snapshots.git"),
	})
	if err != nil {
		t.Fatalf("open snapshot-store: %v", err)
	}
	t.Cleanup(func() { ss.Close() })

	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		ConformancePath: filepath.Join(dir, "conformance.json"),
		LogStoreURL:     "http://localhost:0", // unused here
	}

	// New() calls routes(), which is where a conflicting pattern panics.
	// A panic here fails the test with the conflict named, which is the
	// whole point: it fails at `go test` instead of at boot.
	return New(st, as, ms, hs, hks, mds, ss, cfg)
}
