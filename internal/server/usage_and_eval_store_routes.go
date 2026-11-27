package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/agents/evaluations"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelrouting"
	usagestorehttpapi "github.com/kayushkin/llm-bridge-server/internal/agents/usage/httpapi"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// usage-store and eval-store run in this process from their Postgres modules.
// Their routes are mounted under /usage/ and /evaluations/ with the paths
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
	usageRoutePrefix       = "/usage"
	evaluationsRoutePrefix = "/evaluations"
)

// EnableUsage serves usage-store's routes from handlers, and from then on
// the one-shot router reads subscription limits from it.
func (s *Server) EnableUsage(handlers *usagestorehttpapi.Server) {
	s.usageAndEvalStores.usage.Store(handlers)
}

// usageStoreHandler answers a request whose path has no /usage prefix,
// or 503 while the module is not open.
func (s *Server) usageStoreHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers := s.usageAndEvalStores.usage.Load()
		if handlers == nil {
			httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "usage_store_off", "this server's usage-store module is not open")
			return
		}
		handlers.ServeHTTP(w, r)
	})
}

// errUsageStoreOff is a read of usage-store on a server whose module is not
// open.
var errUsageStoreOff = errors.New("this server's usage-store module is not open")

// readUsageStoreLimits is usage-store's latest limits, read in this process
// from the same function its GET /api/usage/limits answers from.
func (s *Server) readUsageStoreLimits(context.Context) (map[string]modelrouting.UsageProviderLimits, error) {
	handlers := s.usageAndEvalStores.usage.Load()
	if handlers == nil {
		return nil, errUsageStoreOff
	}
	latest, err := handlers.LatestLimits()
	if err != nil {
		return nil, err
	}
	providers := make(map[string]modelrouting.UsageProviderLimits, len(latest))
	for provider, snapshot := range latest {
		if snapshot == nil {
			continue
		}
		limits := modelrouting.UsageProviderLimits{UsedPercentByWindow: make(map[string]float64, len(snapshot.Windows))}
		if snapshot.StaleAfter != nil {
			limits.StaleAfter = *snapshot.StaleAfter
		}
		for name, window := range snapshot.Windows {
			if window != nil {
				limits.UsedPercentByWindow[name] = window.UsedPercent
			}
		}
		providers[provider] = limits
	}
	return providers, nil
}

// EvalStoreOptions are the eval-store settings the runner takes.
type EvalStoreOptions struct {
	// RunsEnabled is evaluations.runs.enabled: off, start and resume answer 503.
	RunsEnabled              bool
	Defaults                 evaluations.RunDefaults
	OperationPollingInterval time.Duration
}

// EnableEvaluations serves eval-store's routes from store and returns the
// runner that carries its runs out, which main closes at shutdown. Every
// model call is an llm.completion operation on this server, and prices and
// subscription usage come from model-store and usage-store, all called in
// process as the internal service (evalStoreInThisServer), as the standalone
// eval-store called them with the service token alone.
func (s *Server) EnableEvaluations(store *evaluations.Store, options EvalStoreOptions) (*evaluations.Runner, error) {
	if options.RunsEnabled && options.OperationPollingInterval <= 0 {
		return nil, fmt.Errorf("eval-store: %s is %s; it must be longer than nothing", config.SettingEvaluationsOperationPollingInterval, options.OperationPollingInterval)
	}
	thisServer := evalStoreInThisServer{server: s}
	bridge := &evaluations.BridgeOperations{Operations: thisServer, PollInterval: options.OperationPollingInterval}
	runner := evaluations.NewRunner(store, bridge, thisServer, thisServer, options.Defaults)
	if !options.RunsEnabled {
		runner.SwitchRunsOff(config.SettingEvaluationsRunsEnabled + " is off on this server")
	}
	routes := http.NewServeMux()
	evaluations.RegisterHandlers(routes, store, runner, bridge)
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
	return storeRouteWithoutPrefix(usageRoutePrefix, s.usageStoreHandler)
}

func (s *Server) evalStoreRoute() http.HandlerFunc {
	return storeRouteWithoutPrefix(evaluationsRoutePrefix, func() http.Handler {
		routes := s.usageAndEvalStores.eval.Load()
		if routes == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "eval_store_off", "this server's eval-store module is not open")
			})
		}
		return routes
	})
}

// registerUsageAndEvalStoreRoutes mounts the two stores. The patterns are
// written out, because the route classification test reads them from string
// literals.
func (s *Server) registerUsageAndEvalStoreRoutes() {
	s.mux.HandleFunc("GET /usage/api/usage", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage/api/usage/limits", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage/api/usage/limits/{provider}", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage/api/usage/limits/{provider}/history", s.usageStoreRoute())
	s.mux.HandleFunc("POST /usage/api/usage/limits/refresh", s.usageStoreRoute())
	s.mux.HandleFunc("POST /usage/api/usage/limits/watch", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage/api/usage/spend/keys", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage/api/usage/spend/keys/{provider}/{api_key_id}/raw", s.usageStoreRoute())
	s.mux.HandleFunc("POST /usage/api/usage/spend/refresh", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage/api/usage/spend/topups", s.usageStoreRoute())
	s.mux.HandleFunc("POST /usage/api/usage/spend/topups", s.usageStoreRoute())
	s.mux.HandleFunc("DELETE /usage/api/usage/spend/topups/{id}", s.usageStoreRoute())
	s.mux.HandleFunc("GET /usage/health", s.usageStoreRoute())

	s.mux.HandleFunc("GET /evaluations/health", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/vocabulary", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/suites", s.evalStoreRoute())
	s.mux.HandleFunc("POST /evaluations/suites", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/suites/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("PATCH /evaluations/suites/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("DELETE /evaluations/suites/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/suites/{id}/cases", s.evalStoreRoute())
	s.mux.HandleFunc("POST /evaluations/suites/{id}/cases", s.evalStoreRoute())
	s.mux.HandleFunc("POST /evaluations/suites/{id}/cases/import-operations", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/cases/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("PATCH /evaluations/cases/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/runs", s.evalStoreRoute())
	s.mux.HandleFunc("POST /evaluations/runs", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/runs/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("PATCH /evaluations/runs/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("DELETE /evaluations/runs/{id}", s.evalStoreRoute())
	s.mux.HandleFunc("POST /evaluations/runs/{id}/start", s.evalStoreRoute())
	s.mux.HandleFunc("POST /evaluations/runs/{id}/resume", s.evalStoreRoute())
	s.mux.HandleFunc("POST /evaluations/runs/{id}/stop", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/runs/{id}/answers", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/runs/{id}/verdicts", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/runs/{id}/checks", s.evalStoreRoute())
	s.mux.HandleFunc("POST /evaluations/runs/{id}/checks", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/runs/{id}/report", s.evalStoreRoute())
	s.mux.HandleFunc("GET /evaluations/runs/{id}/cost-projection", s.evalStoreRoute())
}
