package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	permstore "github.com/kayushkin/llm-bridge-server/internal/access/permissionstore"
)

// --- health -------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"db":     s.store.Path(),
	})
}

// --- rules CRUD ---------------------------------------------------------

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	rules, err := s.store.ListRules(scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rules == nil {
		rules = []permstore.Rule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var rule permstore.Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := validateRule(rule); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := s.store.CreateRule(rule)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.engine.InvalidateCache()
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleGetRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rule, err := s.store.GetRule(id)
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

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetRule(id)
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
	updated, err := s.store.UpdateRule(merged)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.engine.InvalidateCache()
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteRule(id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.engine.InvalidateCache()
	w.WriteHeader(http.StatusNoContent)
}

// --- evaluate -----------------------------------------------------------

func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	var req permstore.EvaluateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("tool is required"))
		return
	}
	resp, err := s.engine.Evaluate(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- audit --------------------------------------------------------------

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	entries, err := s.store.ListAudit(permstore.ListAuditFilter{
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
		entries = []permstore.AuditEntry{}
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
func validateRule(r permstore.Rule) error {
	switch r.Outcome {
	case permstore.OutcomeAllow, permstore.OutcomeDeny, permstore.OutcomeAsk:
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
