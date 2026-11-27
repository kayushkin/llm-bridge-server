package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/agents/hooks/hookstest"
	"github.com/kayushkin/llm-bridge-server/internal/machines/machinestest"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/snapshots/snapshotstest"

	"github.com/kayushkin/llm-bridge-server/internal/config"
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
// TestTheLoginAndStoreProxyRoutesAreMountedAndGated checks the routes the
// login and the two store proxies add meet the rest of the mux, and that a
// caller with no credential is refused on them.
func TestTheLoginAndStoreProxyRoutesAreMountedAndGated(t *testing.T) {
	srv := newServerWithAllStoresConfigured(t, nil)
	for _, route := range []struct{ method, path string }{
		{"GET", "/auth/principal"},
		{"GET", "/issues/boards"},
		{"GET", "/grants/relations"},
		{"GET", "/mail/api/accounts"},
		{"POST", "/references/resolve"},
	} {
		// Through ServeHTTP, not the bare mux: the request authorization in
		// front of the mux is part of the route.
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest(route.method, route.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a login cookie -> %d, want 401", route.method, route.path, w.Code)
		}
	}
}

func newServerWithAllStores(t *testing.T) *Server {
	t.Helper()
	return newServerWithAllStoresConfigured(t, func(*config.Config) {})
}

// newServerWithAllStoresConfigured is newServerWithAllStores with a hook to
// adjust the config before New registers the routes.
func newServerWithAllStoresConfigured(t *testing.T, adjustConfig func(*config.Config)) *Server {
	t.Helper()
	dir := t.TempDir()

	st := newTestBridgeStore(t)

	as := openTestAgentStore(t)
	ms := openTestMemoryStore(t)

	hs := machinestest.NewStore(t)

	hks := hookstest.NewStore(t)

	mds := openTestModelStore(t)

	ss := snapshotstest.NewStore(t)

	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		ConformancePath: filepath.Join(dir, "conformance.json"),
	}
	testAuthorizationConfig(cfg)

	if adjustConfig != nil {
		adjustConfig(cfg)
	}

	// New() calls routes(), which is where a conflicting pattern panics.
	// A panic here fails the test with the conflict named, which is the
	// whole point: it fails at `go test` instead of at boot.
	srv := New(st, as, ms, hs, hks, mds, ss, cfg)
	enableTestSessionEvents(t, srv, st)
	return srv
}
