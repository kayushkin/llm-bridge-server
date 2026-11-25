package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/agents/evalstore"
	usagestorehttpapi "github.com/kayushkin/llm-bridge-server/internal/agents/usagestore/httpapi"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
)

// usage-store and eval-store run in this process from their Postgres modules.
// Their routes are mounted under /usage-store/ and /eval-store/ with the paths
// the standalone services answered, and each request reaches the store's own
// handler with the prefix taken off. The one-shot router reads usage-store's
// limits in process, and eval-store calls this server's operations,
// model-store and usage-store in process.

// usageAndEvalStores holds the two stores' handlers once main has opened
// their modules; until then their routes answer 503.
type usageAndEvalStores struct {
	usage atomic.Pointer[usagestorehttpapi.Server]
	eval  atomic.Pointer[http.ServeMux]
}

const (
	usageStoreRoutePrefix = "/usage-store"
	evalStoreRoutePrefix  = "/eval-store"
)

// EnableUsageStore serves usage-store's routes from handlers, and from then on
// the one-shot router reads subscription limits from it.
func (s *Server) EnableUsageStore(handlers *usagestorehttpapi.Server) {
	s.usageAndEvalStores.usage.Store(handlers)
}

// usageStoreHandler answers a request whose path has no /usage-store prefix,
// or 503 while the module is not open.
func (s *Server) usageStoreHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers := s.usageAndEvalStores.usage.Load()
		if handlers == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "usage_store_off", "this server's usage-store module is not open")
			return
		}
		handlers.ServeHTTP(w, r)
	})
}

// usageStoreLimitsClient is the HTTP client the one-shot router reads
// usage-store's limits with: it never leaves this process. A read is bounded
// as the old one over the network was.
func (s *Server) usageStoreLimitsClient() *http.Client {
	client := inprocesshttp.Client(s.usageStoreHandler())
	client.Timeout = 3 * time.Second
	return client
}

// EvalStoreOptions are the eval-store settings the runner takes.
type EvalStoreOptions struct {
	// RunsEnabled is eval_store.runs.enabled: off, start and resume answer 503.
	RunsEnabled              bool
	Defaults                 evalstore.RunDefaults
	OperationPollingInterval time.Duration
}

// EnableEvalStore serves eval-store's routes from store and returns the
// runner that carries its runs out, which main closes at shutdown. Every
// model call is an llm.completion operation on this server, and prices and
// subscription usage come from model-store and usage-store, all in process
// and as the internal service, as the standalone eval-store called them with
// the service token alone.
func (s *Server) EnableEvalStore(store *evalstore.Store, options EvalStoreOptions) (*evalstore.Runner, error) {
	if options.RunsEnabled && options.OperationPollingInterval <= 0 {
		return nil, fmt.Errorf("eval-store: %s is %s; it must be longer than nothing", config.SettingEvalStoreOperationPollingInterval, options.OperationPollingInterval)
	}
	asInternalService := inprocesshttp.Client(s.mux)
	bridge := &evalstore.BridgeClient{BaseURL: inprocesshttp.BaseURL, HTTP: asInternalService, PollInterval: options.OperationPollingInterval}
	runner := evalstore.NewRunner(store, bridge,
		&evalstore.ModelStoreClient{BaseURL: inprocesshttp.BaseURL + "/model-store", HTTP: asInternalService},
		&evalstore.UsageStoreClient{BaseURL: inprocesshttp.BaseURL + usageStoreRoutePrefix, HTTP: asInternalService},
		options.Defaults)
	if !options.RunsEnabled {
		runner.SwitchRunsOff(config.SettingEvalStoreRunsEnabled + " is off on this server")
	}
	routes := http.NewServeMux()
	evalstore.RegisterHandlers(routes, store, runner, bridge)
	s.usageAndEvalStores.eval.Store(routes)
	return runner, nil
}

// storeRouteWithoutPrefix answers a request with handler, which sees the path
// without prefix, as the standalone store did.
func storeRouteWithoutPrefix(prefix string, handler func() http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inner := r.Clone(r.Context())
		inner.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		inner.URL.RawPath = ""
		handler().ServeHTTP(w, inner)
	}
}

func (s *Server) usageStoreRoute() http.HandlerFunc {
	return storeRouteWithoutPrefix(usageStoreRoutePrefix, s.usageStoreHandler)
}

func (s *Server) evalStoreRoute() http.HandlerFunc {
	return storeRouteWithoutPrefix(evalStoreRoutePrefix, func() http.Handler {
		routes := s.usageAndEvalStores.eval.Load()
		if routes == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSONError(w, http.StatusServiceUnavailable, "eval_store_off", "this server's eval-store module is not open")
			})
		}
		return routes
	})
}

// registerUsageAndEvalStoreRoutes mounts the two stores. The patterns are
// written out, because the route classification test reads them from string
// literals.
func (s *Server) registerUsageAndEvalStoreRoutes() {
	s.mux.HandleFunc("GET /usage-store/api/usage", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage-store/api/usage/limits", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage-store/api/usage/limits/{provider}", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage-store/api/usage/limits/{provider}/history", s.usageStoreRoute())
	s.mux.HandleFunc("POST /usage-store/api/usage/limits/refresh", s.usageStoreRoute())
	s.mux.HandleFunc("POST /usage-store/api/usage/limits/watch", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage-store/api/usage/spend/keys", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage-store/api/usage/spend/keys/{provider}/{api_key_id}/raw", s.usageStoreRoute())
	s.mux.HandleFunc("POST /usage-store/api/usage/spend/refresh", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage-store/api/usage/spend/topups", s.usageStoreRoute())
	s.mux.HandleFunc("POST /usage-store/api/usage/spend/topups", s.usageStoreRoute())
	s.mux.HandleFunc("DELETE /usage-store/api/usage/spend/topups/{id}", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage-store/health", s.usageStoreRoute())

	s.mux.HandleFunc("GET /eval-store/health", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/vocabulary", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/suites", s.evalStoreRoute())
	s.mux.HandleFunc("POST /eval-store/suites", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/suites/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("PATCH /eval-store/suites/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("DELETE /eval-store/suites/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/suites/{id}/cases", s.evalStoreRoute())
	s.mux.HandleFunc("POST /eval-store/suites/{id}/cases", s.evalStoreRoute())
	s.mux.HandleFunc("POST /eval-store/suites/{id}/cases/import-operations", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/cases/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("PATCH /eval-store/cases/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/runs", s.evalStoreRoute())
	s.mux.HandleFunc("POST /eval-store/runs", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/runs/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("PATCH /eval-store/runs/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("DELETE /eval-store/runs/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("POST /eval-store/runs/{id}/start", s.evalStoreRoute())
	s.mux.HandleFunc("POST /eval-store/runs/{id}/resume", s.evalStoreRoute())
	s.mux.HandleFunc("POST /eval-store/runs/{id}/stop", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/runs/{id}/answers", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/runs/{id}/verdicts", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/runs/{id}/checks", s.evalStoreRoute())
	s.mux.HandleFunc("POST /eval-store/runs/{id}/checks", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/runs/{id}/report", s.evalStoreRoute())
	s.mux.HandleFunc("GET /eval-store/runs/{id}/cost-projection", s.evalStoreRoute())
}
