package models

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Handlers answers the registry's HTTP routes, the ones the standalone `ms
// serve` answered on its own port: the bridge mounts each one under
// /model-store/ with the same path behind the prefix.
//
//	GET  /api/models               all models with health, by priority then id
//	POST /api/models/toggle        {"model": "<id>", "enabled": bool}
//	GET  /api/roles                {"roles":{"best":…}, "models":{"best":[…]}, "canonical":[…]}
//	POST /api/roles                {"role":…,"model":…} makes that the first model and keeps
//	                               the fallbacks; {"role":…,"models":[…]} replaces the whole list
//	GET  /api/roles/{role}         the role's first model, resolved
//	GET  /api/roles/{role}/models  every model in the role's list, in the order to try them
//	GET  /api/health               liveness
//
// The standalone server's 410 answers for its removed credential and test
// routes are not carried over: nothing calls those paths under the prefix.
type Handlers struct {
	store *Store
}

// NewHandlers returns the route handlers over store.
func NewHandlers(store *Store) *Handlers { return &Handlers{store: store} }

// ListModels answers GET /api/models.
func (h *Handlers) ListModels(w http.ResponseWriter, r *http.Request) {
	statuses, err := h.store.AllModelsWithStatus()
	if err != nil {
		httpError(w, fmt.Sprintf("failed to query models: %v", err), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, statuses)
}

// ToggleModel answers POST /api/models/toggle.
func (h *Handlers) ToggleModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model   string `json:"model"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		httpError(w, "model required", http.StatusBadRequest)
		return
	}
	if err := h.store.SetEnabled(req.Model, req.Enabled); err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "model": req.Model, "enabled": req.Enabled})
}

// ListRoles answers GET /api/roles: each role's first model, each role's whole
// list, and the canonical role set, so a client never invents it.
func (h *Handlers) ListRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.store.Roles()
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	lists, err := h.store.RoleModelLists()
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"roles": roles, "models": lists, "canonical": CanonicalRoles})
}

// SetRole answers POST /api/roles. Sending both "model" and "models" is a 400.
func (h *Handlers) SetRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role   string   `json:"role"`
		Model  string   `json:"model"`
		Models []string `json:"models"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var err error
	switch {
	case req.Model != "" && req.Models != nil:
		httpError(w, `send "model" or "models", not both`, http.StatusBadRequest)
		return
	case req.Models != nil:
		err = h.store.SetRoleModels(req.Role, req.Models)
	case req.Model != "":
		err = h.store.SetRole(req.Role, req.Model)
	default:
		httpError(w, `"model" or "models" is required`, http.StatusBadRequest)
		return
	}
	if err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	list, err := h.store.RoleModelIDs(req.Role)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "role": req.Role, "model": list[0], "models": list})
}

// GetRole answers GET /api/roles/{role} with the role's first model, resolved.
func (h *Handlers) GetRole(w http.ResponseWriter, r *http.Request) {
	m, err := h.store.ResolveRole(r.PathValue("role"))
	if err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, m)
}

// GetRoleModels answers GET /api/roles/{role}/models with every model in the
// role's list, in the order to try them.
func (h *Handlers) GetRoleModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.store.RoleModels(r.PathValue("role"))
	if err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, models)
}

// Health answers GET /api/health.
func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, map[string]any{"ok": true})
}

// Register mounts every route on mux under prefix, which is "" for a mux of
// the registry's own and "/model-store" in the bridge.
func (h *Handlers) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/api/models", h.ListModels)
	mux.HandleFunc("POST "+prefix+"/api/models/toggle", h.ToggleModel)
	mux.HandleFunc("GET "+prefix+"/api/roles", h.ListRoles)
	mux.HandleFunc("POST "+prefix+"/api/roles", h.SetRole)
	mux.HandleFunc("GET "+prefix+"/api/roles/{role}", h.GetRole)
	mux.HandleFunc("GET "+prefix+"/api/roles/{role}/models", h.GetRoleModels)
	mux.HandleFunc("GET "+prefix+"/api/health", h.Health)
}

func httpError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
