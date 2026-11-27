package server

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/predictions"
)

// The calibration ledger, moved in from the standalone prediction-store: the
// same routes under /prediction-store/, answered from the Postgres module.

// EnablePredictionStore serves the store's routes from store. main calls it
// once the module is provisioned, migrated and open; until then, and on a
// server with no Postgres URL, the routes answer 503.
func (s *Server) EnablePredictionStore(store *predictions.Store) {
	s.predictionStore.Store(predictions.NewHandlers(store))
	s.referenceReadable.predictions.Store(store)
}

// predictionStoreRoute answers one route with the store's handler for it.
func (s *Server) predictionStoreRoute(route func(*predictions.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers := s.predictionStore.Load()
		if handlers == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "prediction_store_off",
				config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no predictions")
			return
		}
		route(handlers, w, r)
	}
}

// registerPredictionStoreRoutes mounts the store. The patterns are written out,
// because the route classification test reads them from string literals.
func (s *Server) registerPredictionStoreRoutes() {
	s.mux.HandleFunc("GET /prediction-store/health", s.predictionStoreRoute((*predictions.Handlers).Health))
	s.mux.HandleFunc("GET /prediction-store/vocabulary", s.predictionStoreRoute((*predictions.Handlers).Vocabulary))
	s.mux.HandleFunc("GET /prediction-store/categories", s.predictionStoreRoute((*predictions.Handlers).Vocabulary))
	s.mux.HandleFunc("GET /prediction-store/tags", s.predictionStoreRoute((*predictions.Handlers).ListTags))
	s.mux.HandleFunc("GET /prediction-store/calibration", s.predictionStoreRoute((*predictions.Handlers).Calibration))
	s.mux.HandleFunc("GET /prediction-store/base-rates", s.predictionStoreRoute((*predictions.Handlers).BaseRates))
	s.mux.HandleFunc("GET /prediction-store/predictions", s.predictionStoreRoute((*predictions.Handlers).ListPredictions))
	s.mux.HandleFunc("POST /prediction-store/predictions", s.predictionStoreRoute((*predictions.Handlers).CreatePrediction))
	s.mux.HandleFunc("GET /prediction-store/predictions/{id}", s.predictionStoreRoute((*predictions.Handlers).GetPrediction))
	s.mux.HandleFunc("PATCH /prediction-store/predictions/{id}", s.predictionStoreRoute((*predictions.Handlers).PatchPrediction))
	s.mux.HandleFunc("DELETE /prediction-store/predictions/{id}", s.predictionStoreRoute((*predictions.Handlers).DeletePrediction))
	s.mux.HandleFunc("POST /prediction-store/predictions/{id}/restore", s.predictionStoreRoute((*predictions.Handlers).RestorePrediction))
	s.mux.HandleFunc("GET /prediction-store/predictions/{id}/estimates", s.predictionStoreRoute((*predictions.Handlers).ListEstimates))
	s.mux.HandleFunc("POST /prediction-store/predictions/{id}/estimates", s.predictionStoreRoute((*predictions.Handlers).AddEstimate))
	s.mux.HandleFunc("POST /prediction-store/predictions/{id}/resolve", s.predictionStoreRoute((*predictions.Handlers).ResolvePrediction))
	s.mux.HandleFunc("GET /prediction-store/predictions/{id}/links", s.predictionStoreRoute((*predictions.Handlers).ListLinks))
	s.mux.HandleFunc("POST /prediction-store/predictions/{id}/links", s.predictionStoreRoute((*predictions.Handlers).AddLink))
	s.mux.HandleFunc("DELETE /prediction-store/links/{id}", s.predictionStoreRoute((*predictions.Handlers).DeleteLink))
}
