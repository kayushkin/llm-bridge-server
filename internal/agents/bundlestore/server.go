package bundlestore

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// RegisterHandlers wires the HTTP API onto mux.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}

	mux.HandleFunc("GET /health", h.health)

	mux.HandleFunc("GET /bundles", h.listBundles)
	mux.HandleFunc("POST /bundles", h.upsertBundle)
	mux.HandleFunc("GET /bundles/{id}", h.getBundle)
	mux.HandleFunc("DELETE /bundles/{id}", h.deleteBundle)
	mux.HandleFunc("POST /bundles/{id}/enable", h.enableBundle)
	mux.HandleFunc("POST /bundles/{id}/disable", h.disableBundle)
	mux.HandleFunc("GET /bundles/by-name/{name}", h.getBundleByName)

	mux.HandleFunc("GET /member-effects", h.memberEffects)

	mux.HandleFunc("POST /resolve", h.resolve)
}

type handler struct{ s *Store }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// memberEffects serves MemberEffects, the one list a member's effect is
// validated against.
func (h *handler) memberEffects(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, MemberEffects)
}

func (h *handler) listBundles(w http.ResponseWriter, r *http.Request) {
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
	bundles, err := h.s.ListBundles(f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if bundles == nil {
		bundles = []Bundle{}
	}
	writeJSON(w, 200, bundles)
}

func (h *handler) upsertBundle(w http.ResponseWriter, r *http.Request) {
	var b Bundle
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	inserted, err := h.s.UpsertBundle(&b)
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

func (h *handler) getBundle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	b, err := h.s.GetBundle(id)
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, b)
}

func (h *handler) getBundleByName(w http.ResponseWriter, r *http.Request) {
	b, err := h.s.GetBundleByName(r.PathValue("name"))
	if err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, b)
}

func (h *handler) deleteBundle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := h.s.DeleteBundle(id); err != nil {
		if errors.Is(err, ErrParentHasChildren) {
			writeErr(w, 409, err.Error())
			return
		}
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (h *handler) enableBundle(w http.ResponseWriter, r *http.Request)  { h.setEnabled(w, r, true) }
func (h *handler) disableBundle(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, false) }

func (h *handler) setEnabled(w http.ResponseWriter, r *http.Request, v bool) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := h.s.SetEnabled(id, v); err != nil {
		respondLookup(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "enabled": v})
}

func (h *handler) resolve(w http.ResponseWriter, r *http.Request) {
	var req ResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	resp, err := Resolve(h.s, req)
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
