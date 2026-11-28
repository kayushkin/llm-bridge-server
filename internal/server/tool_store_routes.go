package server

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/sessions"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/agents/tools"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// The registry of tools a harness can be given, moved in from the standalone
// tool-store: the same routes under /tools/, answered from the Postgres
// module, and the questions session creation and spawn ask it, answered in
// process through ToolRegistry.

// EnableTools serves the store's routes from store and answers the tool
// offer, the harness tool denials and the harness tool reports from it.
// resolveCredential turns the auth-store provider an MCP tool's environment
// key names into its secret when the tool is provisioned. main calls it once
// the module is provisioned, migrated, open and seeded, before the server
// takes requests; until then, and on a server with no Postgres URL, the routes
// answer 503 and sessions get no tools from the registry.
func (s *Server) EnableTools(store *tools.Store, resolveCredential tools.ResolveCredentialFunc) {
	s.toolRegistry = sessions.InProcessToolRegistry{Store: store, ResolveCredential: resolveCredential}
	s.toolStoreForResourceChecks.Store(store)
	s.toolStoreHandlers.Store(tools.NewHandlers(store, tools.HandlerOptions{
		ResolveCredential: resolveCredential,
		InvokeLocal:       tools.InvokeRegisteredLocalTool,
		ListLocals:        tools.RegisteredLocalTools,
	}))
}

// ToolStoreCredentialResolver is the resolver EnableTools is given on this
// server: auth-store, through the server's own auth-store client.
func (s *Server) ToolStoreCredentialResolver() tools.ResolveCredentialFunc {
	return sessions.CredentialResolverFromAuthStore(s.authClient)
}

// toolStoreRoute answers one route with the store's handler for it.
func (s *Server) toolStoreRoute(route func(*tools.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers := s.toolStoreHandlers.Load()
		if handlers == nil {
			httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "tool_store_off",
				config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no tool registry")
			return
		}
		route(handlers, w, r)
	}
}

// registerToolStoreRoutes mounts the store. The patterns are written out,
// because the route classification test reads them from string literals.
func (s *Server) registerToolStoreRoutes() {
	s.mux.HandleFunc("GET /tools/health", s.toolStoreRoute((*tools.Handlers).Health))
	s.mux.HandleFunc("GET /tools/kinds", s.toolStoreRoute((*tools.Handlers).ListKinds))
	s.mux.HandleFunc("GET /tools/tools", s.toolStoreRoute((*tools.Handlers).ListTools))
	s.mux.HandleFunc("POST /tools/tools", s.toolStoreRoute((*tools.Handlers).UpsertTool))
	s.mux.HandleFunc("GET /tools/tools/{id}", s.toolStoreRoute((*tools.Handlers).GetTool))
	s.mux.HandleFunc("PATCH /tools/tools/{id}", s.toolStoreRoute((*tools.Handlers).PatchTool))
	s.mux.HandleFunc("DELETE /tools/tools/{id}", s.toolStoreRoute((*tools.Handlers).DeleteTool))
	s.mux.HandleFunc("POST /tools/tools/{id}/enable", s.toolStoreRoute((*tools.Handlers).EnableTool))
	s.mux.HandleFunc("POST /tools/tools/{id}/disable", s.toolStoreRoute((*tools.Handlers).DisableTool))
	s.mux.HandleFunc("GET /tools/tools/by-name/{name}", s.toolStoreRoute((*tools.Handlers).GetToolByName))
	s.mux.HandleFunc("GET /tools/tools/by-name/{name}/spec", s.toolStoreRoute((*tools.Handlers).SpecByName))
	s.mux.HandleFunc("POST /tools/tools/by-name/{name}/invoke", s.toolStoreRoute((*tools.Handlers).InvokeByName))
	s.mux.HandleFunc("GET /tools/tools/by-name/{name}/instances", s.toolStoreRoute((*tools.Handlers).ListInstancesForTool))
	s.mux.HandleFunc("POST /tools/harness-tools/observed", s.toolStoreRoute((*tools.Handlers).RecordObservedHarnessTools))
	s.mux.HandleFunc("GET /tools/locals", s.toolStoreRoute((*tools.Handlers).ListLocals))
	s.mux.HandleFunc("POST /tools/provision", s.toolStoreRoute((*tools.Handlers).Provision))
	s.mux.HandleFunc("GET /tools/instances/{id}/tools", s.toolStoreRoute((*tools.Handlers).ListInstanceTools))
	s.mux.HandleFunc("POST /tools/instances/{id}/tools/by-name/{name}", s.toolStoreRoute((*tools.Handlers).EnableForInstance))
	s.mux.HandleFunc("DELETE /tools/instances/{id}/tools/by-name/{name}", s.toolStoreRoute((*tools.Handlers).DisableForInstance))
}
