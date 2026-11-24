package permissionstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Handlers answers permission-store's routes: GET /healthz, GET and POST
// /rules, GET, PATCH and DELETE /rules/{id}, POST /evaluate and GET /audit.
// The server it is mounted in registers each under its own prefix and decides
// who may call them; these handlers check no caller.
type Handlers struct {
	store  *Store
	engine *Engine
}

// NewHandlers answers the routes from store. engine must be the one the
// server's own tool-call gate evaluates with, because a rule write here drops
// its compiled patterns.
func NewHandlers(store *Store, engine *Engine) *Handlers {
	if store == nil || engine == nil {
		panic("permission-store: NewHandlers needs a Store and the Engine the server evaluates with")
	}
	return &Handlers{store: store, engine: engine}
}

// --- health -------------------------------------------------------------

// Health answers {"status":"ok","rules":N}. The standalone store named its
// database file here; a module has none.
func (s *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.ListRules(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"rules":  len(rules),
	})
}

// --- rules CRUD ---------------------------------------------------------

// ListRules answers its route.
func (s *Handlers) ListRules(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	rules, err := s.store.ListRules(r.Context(), scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rules == nil {
		rules = []Rule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

// CreateRule answers its route.
func (s *Handlers) CreateRule(w http.ResponseWriter, r *http.Request) {
	var rule Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := validateRule(rule); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := s.store.CreateRule(r.Context(), rule)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.engine.InvalidateCache()
	writeJSON(w, http.StatusCreated, created)
}

// GetRule answers its route.
func (s *Handlers) GetRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rule, err := s.store.GetRule(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Errorf("rule %q not found", id))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// UpdateRule answers its route.
func (s *Handlers) UpdateRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetRule(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Errorf("rule %q not found", id))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// PATCH semantics: decode into a map, apply only present keys to the
	// existing struct. This avoids the "all-empty fields wipe the row"
	// trap that comes from decoding into a typed struct directly.
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	merged := existing
	for k, v := range patch {
		switch k {
		case "scope":
			_ = json.Unmarshal(v, &merged.Scope)
		case "priority":
			_ = json.Unmarshal(v, &merged.Priority)
		case "tool":
			_ = json.Unmarshal(v, &merged.Tool)
		case "pattern":
			_ = json.Unmarshal(v, &merged.Pattern)
		case "outcome":
			_ = json.Unmarshal(v, &merged.Outcome)
		case "message":
			_ = json.Unmarshal(v, &merged.Message)
		case "enabled":
			_ = json.Unmarshal(v, &merged.Enabled)
		}
	}
	merged.ID = id
	if err := validateRule(merged); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	updated, err := s.store.UpdateRule(r.Context(), merged)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.engine.InvalidateCache()
	writeJSON(w, http.StatusOK, updated)
}

// DeleteRule answers its route.
func (s *Handlers) DeleteRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteRule(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.engine.InvalidateCache()
	w.WriteHeader(http.StatusNoContent)
}

// --- evaluate -----------------------------------------------------------

// Evaluate answers its route.
func (s *Handlers) Evaluate(w http.ResponseWriter, r *http.Request) {
	var req EvaluateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("tool is required"))
		return
	}
	resp, err := s.engine.Evaluate(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- audit --------------------------------------------------------------

// ListAudit answers its route.
func (s *Handlers) ListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	entries, err := s.store.ListAudit(r.Context(), ListAuditFilter{
		BridgeID:   q.Get("bridge_id"),
		InstanceID: q.Get("instance_id"),
		Tool:       q.Get("tool"),
		Since:      q.Get("since"),
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if entries == nil {
		entries = []AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// --- helpers ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Response already started — log and move on.
		fmt.Printf("[permission-store] write json: %v\n", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// validateRule enforces field semantics the SQL layer can't (CHECK
// constraints would work too, but Go-side gives nicer error messages).
func validateRule(r Rule) error {
	switch r.Outcome {
	case OutcomeAllow, OutcomeDeny, OutcomeAsk:
	default:
		return fmt.Errorf("outcome must be allow / deny / ask, got %q", r.Outcome)
	}
	if r.Tool == "" {
		return fmt.Errorf("tool is required (use \"*\" to match any tool)")
	}
	if r.Scope == "" {
		return fmt.Errorf("scope is required (global, bridge:<id>, instance:<id>)")
	}
	return nil
}
