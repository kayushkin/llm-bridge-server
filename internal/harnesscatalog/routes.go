package harnesscatalog

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the harness catalog and health routes on the server's mux: /health, the harnesses and their capabilities and agents, and the conformance matrix.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("GET /health", s.HandleHealth)
	mux.HandleFunc("GET /harnesses", s.HandleHarnesses)
	mux.HandleFunc("GET /harnesses/{name}/capabilities", s.HandleHarnessCapabilities)
	mux.HandleFunc("GET /harnesses/{name}/agents", s.HandleHarnessAgents)
	mux.HandleFunc("GET /conformance", s.HandleConformanceGet)
	mux.HandleFunc("POST /conformance/run", s.HandleConformanceRun)
}
