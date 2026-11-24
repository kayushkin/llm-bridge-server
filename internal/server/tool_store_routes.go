package server

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/agents/toolstore"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// The registry of tools a harness can be given, moved in from the standalone
// tool-store: the same routes under /tool-store/, answered from the Postgres
// module, and the questions session creation and spawn ask it, answered in
// process through toolRegistry.

// EnableToolStore serves the store's routes from store and answers the tool
// offer, the harness tool denials and the harness tool reports from it.
// resolveCredential turns the auth-store provider an MCP tool's environment
// key names into its secret when the tool is provisioned. main calls it once
// the module is provisioned, migrated, open and seeded, before the server
// takes requests; until then, and on a server with no Postgres URL, the routes
// answer 503 and sessions get no tools from the registry.
func (s *Server) EnableToolStore(store *toolstore.Store, resolveCredential toolstore.ResolveCredentialFunc) {
	s.toolRegistry = inProcessToolRegistry{store: store, resolveCredential: resolveCredential}
	s.toolStoreForResourceChecks.Store(store)
	s.toolStoreHandlers.Store(toolstore.NewHandlers(store, toolstore.HandlerOptions{
		ResolveCredential: resolveCredential,
		InvokeLocal:       toolstore.InvokeRegisteredLocalTool,
		ListLocals:        toolstore.RegisteredLocalTools,
	}))
}

// ToolStoreCredentialResolver is the resolver EnableToolStore is given on this
// server: auth-store, through the server's own auth-store client.
func (s *Server) ToolStoreCredentialResolver() toolstore.ResolveCredentialFunc {
	return credentialResolverFromAuthStore(s.authClient)
}

// toolStoreRoute answers one route with the store's handler for it.
func (s *Server) toolStoreRoute(route func(*toolstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers := s.toolStoreHandlers.Load()
		if handlers == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "tool_store_off",
				config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no tool registry")
			return
		}
		route(handlers, w, r)
	}
}

// registerToolStoreRoutes mounts the store. The patterns are written out,
// because the route classification test reads them from string literals.
func (s *Server) registerToolStoreRoutes() {
	s.mux.HandleFunc("GET /tool-store/health", s.toolStoreRoute((*toolstore.Handlers).Health))
	s.mux.HandleFunc("GET /tool-store/kinds", s.toolStoreRoute((*toolstore.Handlers).ListKinds))
	s.mux.HandleFunc("GET /tool-store/tools", s.toolStoreRoute((*toolstore.Handlers).ListTools))
	s.mux.HandleFunc("POST /tool-store/tools", s.toolStoreRoute((*toolstore.Handlers).UpsertTool))
	s.mux.HandleFunc("GET /tool-store/tools/{id}", s.toolStoreRoute((*toolstore.Handlers).GetTool))
	s.mux.HandleFunc("PATCH /tool-store/tools/{id}", s.toolStoreRoute((*toolstore.Handlers).PatchTool))
	s.mux.HandleFunc("DELETE /tool-store/tools/{id}", s.toolStoreRoute((*toolstore.Handlers).DeleteTool))
	s.mux.HandleFunc("POST /tool-store/tools/{id}/enable", s.toolStoreRoute((*toolstore.Handlers).EnableTool))
	s.mux.HandleFunc("POST /tool-store/tools/{id}/disable", s.toolStoreRoute((*toolstore.Handlers).DisableTool))
	s.mux.HandleFunc("GET /tool-store/tools/by-name/{name}", s.toolStoreRoute((*toolstore.Handlers).GetToolByName))
	s.mux.HandleFunc("GET /tool-store/tools/by-name/{name}/spec", s.toolStoreRoute((*toolstore.Handlers).SpecByName))
	s.mux.HandleFunc("POST /tool-store/tools/by-name/{name}/invoke", s.toolStoreRoute((*toolstore.Handlers).InvokeByName))
	s.mux.HandleFunc("GET /tool-store/tools/by-name/{name}/instances", s.toolStoreRoute((*toolstore.Handlers).ListInstancesForTool))
	s.mux.HandleFunc("POST /tool-store/harness-tools/observed", s.toolStoreRoute((*toolstore.Handlers).RecordObservedHarnessTools))
	s.mux.HandleFunc("GET /tool-store/locals", s.toolStoreRoute((*toolstore.Handlers).ListLocals))
	s.mux.HandleFunc("POST /tool-store/provision", s.toolStoreRoute((*toolstore.Handlers).Provision))
	s.mux.HandleFunc("GET /tool-store/instances/{id}/tools", s.toolStoreRoute((*toolstore.Handlers).ListInstanceTools))
	s.mux.HandleFunc("POST /tool-store/instances/{id}/tools/by-name/{name}", s.toolStoreRoute((*toolstore.Handlers).EnableForInstance))
	s.mux.HandleFunc("DELETE /tool-store/instances/{id}/tools/by-name/{name}", s.toolStoreRoute((*toolstore.Handlers).DisableForInstance))
}
