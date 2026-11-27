package server

import (
	"context"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/agents/bundles"
	"github.com/kayushkin/llm-bridge-server/internal/bundleclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// The registry of session bundles, moved in from the standalone bundle-store:
// the same routes under /bundle-store/, answered from the Postgres module, and
// the two questions session creation and spawn ask it, answered in process.

// bundleResolver is what session creation and spawn ask about a bundle.
// bundleclient.Client answers it from the module; nil is a server whose
// bundle-store module is off.
type bundleResolver interface {
	CheckExists(ctx context.Context, bundleID string) error
	Resolve(ctx context.Context, bundleID string) (*bundleclient.Resolution, error)
}

// bundleStoreOffMessage says why a server has no bundle-store.
const bundleStoreOffMessage = "this server's bundle-store module is off (" + config.PostgresURLEnvironmentVariable + " is not set)"

// EnableBundleStore serves the store's routes from store and answers session
// bundles from it. main calls it once the module is provisioned, migrated and
// open, before the server takes requests; until then, and on a server with no
// Postgres URL, the routes answer 503 and a session naming a bundle is refused.
func (s *Server) EnableBundleStore(store *bundles.Store) {
	s.bundleClient = bundleclient.New(store)
	s.bundleStoreHandlers.Store(bundles.NewHandlers(store))
}

// bundleStoreRoute answers one route with the store's handler for it.
func (s *Server) bundleStoreRoute(route func(*bundles.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers := s.bundleStoreHandlers.Load()
		if handlers == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "bundle_store_off",
				config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no bundles")
			return
		}
		route(handlers, w, r)
	}
}

// registerBundleStoreRoutes mounts the store. The patterns are written out,
// because the route classification test reads them from string literals.
func (s *Server) registerBundleStoreRoutes() {
	s.mux.HandleFunc("GET /bundle-store/health", s.bundleStoreRoute((*bundles.Handlers).Health))
	s.mux.HandleFunc("GET /bundle-store/member-effects", s.bundleStoreRoute((*bundles.Handlers).MemberEffects))
	s.mux.HandleFunc("GET /bundle-store/bundles", s.bundleStoreRoute((*bundles.Handlers).ListBundles))
	s.mux.HandleFunc("POST /bundle-store/bundles", s.bundleStoreRoute((*bundles.Handlers).UpsertBundle))
	s.mux.HandleFunc("GET /bundle-store/bundles/{id}", s.bundleStoreRoute((*bundles.Handlers).GetBundle))
	s.mux.HandleFunc("DELETE /bundle-store/bundles/{id}", s.bundleStoreRoute((*bundles.Handlers).DeleteBundle))
	s.mux.HandleFunc("POST /bundle-store/bundles/{id}/enable", s.bundleStoreRoute((*bundles.Handlers).EnableBundle))
	s.mux.HandleFunc("POST /bundle-store/bundles/{id}/disable", s.bundleStoreRoute((*bundles.Handlers).DisableBundle))
	s.mux.HandleFunc("GET /bundle-store/bundles/by-name/{name}", s.bundleStoreRoute((*bundles.Handlers).GetBundleByName))
	s.mux.HandleFunc("POST /bundle-store/resolve", s.bundleStoreRoute((*bundles.Handlers).Resolve))
}
