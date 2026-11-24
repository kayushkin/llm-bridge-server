package memorystore

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// RegisterHandlers registers all HTTP handlers on the given mux.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	h := &handler{s: s}

	mux.HandleFunc("POST /memories", h.save)
	mux.HandleFunc("GET /memories/{id}", h.get)
	mux.HandleFunc("DELETE /memories/{id}", h.forget)
	mux.HandleFunc("POST /memories/search", h.search)
	mux.HandleFunc("GET /memories/recent", h.recent)
	mux.HandleFunc("POST /memories/decay", h.decay)
	mux.HandleFunc("POST /memories/compact", h.compact)
	mux.HandleFunc("POST /memories/context", h.buildContext)
}

type handler struct {
	s *Store
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *handler) save(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID           string    `json:"id"`
		Content      string    `json:"content"`
		Summary      string    `json:"summary,omitempty"`
		Tags         []string  `json:"tags,omitempty"`
		Importance   float64   `json:"importance,omitempty"`
		Source       string    `json:"source,omitempty"`
		AlwaysLoad   bool      `json:"always_load,omitempty"`
		ExpiresAt    *int64    `json:"expires_at,omitempty"` // unix timestamp
		RefType      string    `json:"ref_type,omitempty"`
		RefTarget    string    `json:"ref_target,omitempty"`
		IsLazy       bool      `json:"is_lazy,omitempty"`
		Orchestrator string    `json:"orchestrator,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if req.Content == "" && !req.IsLazy {
		writeErr(w, 400, "content required (unless is_lazy=true)")
		return
	}

	m := Memory{
		ID:           req.ID,
		Content:      req.Content,
		Summary:      req.Summary,
		Tags:         req.Tags,
		Importance:   req.Importance,
		Source:       req.Source,
		AlwaysLoad:   req.AlwaysLoad,
		RefType:      req.RefType,
		RefTarget:    req.RefTarget,
		IsLazy:       req.IsLazy,
		Orchestrator: req.Orchestrator,
	}
	if req.ExpiresAt != nil {
		t := time.Unix(*req.ExpiresAt, 0)
		m.ExpiresAt = &t
	}
	if m.Importance == 0 {
		m.Importance = 0.5
	}
	// A caller that names no source gets no source. This used to default to
	// "user", which is not a neutral choice: "user" is the most trusted
	// provenance a memory can carry, so every write that simply omitted the
	// field was recorded as something the user had said. The store cannot know
	// who a caller is speaking for, and guessing the top of the trust order is
	// the wrong way to be wrong.
	//
	// Importance above is a genuine default — 0.5 is the middle of its range
	// and says nothing. There is no middle of the trust order, so there is
	// nothing for Source to default to.

	if err := h.s.Save(m); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": m.ID, "status": "saved"})
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := h.s.Get(id)
	if err != nil {
		writeErr(w, 404, "memory not found")
		return
	}
	writeJSON(w, 200, m)
}

func (h *handler) forget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.s.Forget(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query        string `json:"query"`
		Limit        int    `json:"limit,omitempty"`
		Orchestrator string `json:"orchestrator,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	var memories []Memory
	var err error
	if req.Orchestrator != "" {
		memories, err = h.s.SearchFiltered(req.Query, req.Limit, req.Orchestrator)
	} else {
		memories, err = h.s.Search(req.Query, req.Limit)
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if memories == nil {
		memories = []Memory{}
	}
	writeJSON(w, 200, memories)
}

func (h *handler) recent(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	minImportance := 0.0
	if mi := r.URL.Query().Get("min_importance"); mi != "" {
		if f, err := strconv.ParseFloat(mi, 64); err == nil {
			minImportance = f
		}
	}

	memories, err := h.s.ListRecent(limit, minImportance)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if memories == nil {
		memories = []Memory{}
	}
	writeJSON(w, 200, memories)
}

func (h *handler) decay(w http.ResponseWriter, r *http.Request) {
	if err := h.s.DecayImportance(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "decayed"})
}

func (h *handler) compact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MinAgeSec int64 `json:"min_age_sec,omitempty"`
		MinCount  int   `json:"min_count,omitempty"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if req.MinAgeSec == 0 {
		req.MinAgeSec = 86400 // 1 day default
	}
	if req.MinCount == 0 {
		req.MinCount = 3
	}

	results, err := h.s.Compact(time.Duration(req.MinAgeSec)*time.Second, req.MinCount)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if results == nil {
		results = []CompactionResult{}
	}
	writeJSON(w, 200, results)
}

func (h *handler) buildContext(w http.ResponseWriter, r *http.Request) {
	var req BuildContextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}

	memories, tokens, err := h.s.BuildContext(req)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if memories == nil {
		memories = []Memory{}
	}
	writeJSON(w, 200, map[string]any{
		"memories": memories,
		"tokens":   tokens,
	})
}
