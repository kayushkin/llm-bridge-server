package inventoryroutes

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the Services page routes on the server's mux: healthcheck's services joined to the SQLite files their processes hold open, and read-only reads of those files.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("GET /services", s.HandleListServices)
	mux.HandleFunc("GET /services/databases/schema", s.HandleDatabaseSchema)
	mux.HandleFunc("GET /services/databases/rows", s.HandleDatabaseRows)
}
