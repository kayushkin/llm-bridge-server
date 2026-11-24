package bundlestore

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// Handlers answers the store's HTTP routes. The server that mounts the store
// registers each method under its own pattern, behind its own prefix and
// access rules; the paths in the method comments are relative to that prefix.
type Handlers struct{ s *Store }

// NewHandlers returns the route handlers of store.
func NewHandlers(store *Store) *Handlers { return &Handlers{s: store} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Health answers GET /health.
func (h *Handlers) Health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// MemberEffects answers GET /member-effects with MemberEffects, the one list
// a member's effect is validated against.
func (h *Handlers) MemberEffects(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, MemberEffects)
}

// ListBundles answers GET /bundles, filtered by ?tag=, ?enabled=true and
// ?limit=.
func (h *Handlers) ListBundles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{Tag: q.Get("tag"), EnabledOnly: q.Get("enabled") == "true"}
	if lim := q.Get("limit"); lim != "" {
		n, err := strconv.Atoi(lim)
		if err != nil {
			writeErr(w, 400, "invalid limit")
			return
		}
		f.Limit = n
	}
	bundles, err := h.s.ListBundles(r.Context(), f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if bundles == nil {
		bundles = []Bundle{}
	}
	writeJSON(w, 200, bundles)
}

// UpsertBundle answers POST /bundles: 201 for a new bundle, 200 for one
// replaced by name.
func (h *Handlers) UpsertBundle(w http.ResponseWriter, r *http.Request) {
	var b Bundle
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	inserted, err := h.s.UpsertBundle(r.Context(), &b)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	status := 200
	if inserted {
		status = 201
	}
	writeJSON(w, status, b)
}

// GetBundle answers GET /bundles/{id}.
func (h *Handlers) GetBundle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	b, err := h.s.GetBundle(r.Context(), id)
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, b)
}

// GetBundleByName answers GET /bundles/by-name/{name}.
func (h *Handlers) GetBundleByName(w http.ResponseWriter, r *http.Request) {
	b, err := h.s.GetBundleByName(r.Context(), r.PathValue("name"))
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, b)
}

// DeleteBundle answers DELETE /bundles/{id}; 409 while other bundles extend it.
func (h *Handlers) DeleteBundle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := h.s.DeleteBundle(r.Context(), id); err != nil {
		if errors.Is(err, ErrParentHasChildren) {
			writeErr(w, 409, err.Error())
			return
		}
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// EnableBundle answers POST /bundles/{id}/enable.
func (h *Handlers) EnableBundle(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, true) }

// DisableBundle answers POST /bundles/{id}/disable.
func (h *Handlers) DisableBundle(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, false) }

func (h *Handlers) setEnabled(w http.ResponseWriter, r *http.Request, v bool) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := h.s.SetEnabled(r.Context(), id, v); err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "enabled": v})
}

// Resolve answers POST /resolve: 404 for a requested bundle id that is
// missing or disabled, 400 for any other refusal.
func (h *Handlers) Resolve(w http.ResponseWriter, r *http.Request) {
	var req ResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	resp, err := Resolve(r.Context(), h.s, req)
	if errors.Is(err, ErrBundleNotApplicable) {
		writeErr(w, 404, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func respondLookup(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeErr(w, 404, "bundle not found")
		return
	}
	writeErr(w, 500, err.Error())
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}
