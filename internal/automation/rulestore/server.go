package rulestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server serves the API. The routes take no credential, so the unit binds
// loopback only.
type Server struct {
	// lifetime is the process's context, for work a request starts and does
	// not wait for.
	lifetime context.Context
	store    *Store
	reactor  *Reactor
	kanban   *KanbanStore
}

// RegisterHandlers mounts the API on mux.
func RegisterHandlers(lifetime context.Context, mux *http.ServeMux, store *Store, reactor *Reactor, kanban *KanbanStore) {
	s := &Server{lifetime: lifetime, store: store, reactor: reactor, kanban: kanban}
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /vocabulary", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, CurrentVocabulary()) })
	mux.HandleFunc("GET /rules", s.listRules)
	mux.HandleFunc("POST /rules", s.createRule)
	mux.HandleFunc("GET /rules/{id}", s.getRule)
	mux.HandleFunc("POST /rules/{id}/revisions", s.addRevision)
	mux.HandleFunc("POST /rules/{id}/activate", s.activate)
	mux.HandleFunc("POST /rules/{id}/pause", s.pause)
	mux.HandleFunc("POST /rules/{id}/retire", s.retire)
	mux.HandleFunc("POST /rules/{id}/preview", s.preview)
	mux.HandleFunc("GET /rules/{id}/comparison", s.comparison)
	mux.HandleFunc("GET /runs", s.listRuns)
	mux.HandleFunc("GET /subjects/{type}/{id}", s.getSubject)
	mux.HandleFunc("GET /activity", s.listActivity)
	mux.HandleFunc("GET /summary", s.summary)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.Counts()
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "counts": counts, "reactor": s.reactor.Status()})
}

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.ListRules(r.URL.Query().Get("state"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

// CreateRuleRequest is the body of POST /rules.
type CreateRuleRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	CreatedBy   string `json:"created_by"`
}

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	var body CreateRuleRequest
	if !decode(w, r, &body) {
		return
	}
	rule, err := s.store.CreateRule(body.Name, body.Description, body.Source, body.CreatedBy)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (s *Server) getRule(w http.ResponseWriter, r *http.Request) {
	rule, err := s.store.GetRule(r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// AddRevisionRequest is the body of POST /rules/{id}/revisions.
type AddRevisionRequest struct {
	Definition json.RawMessage `json:"definition"`
	Note       string          `json:"note"`
	CreatedBy  string          `json:"created_by"`
}

func (s *Server) addRevision(w http.ResponseWriter, r *http.Request) {
	var body AddRevisionRequest
	if !decode(w, r, &body) {
		return
	}
	revision, err := s.store.AddRevision(r.PathValue("id"), body.Definition, body.Note, body.CreatedBy)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, revision)
}

// ActivateRequest is the body of POST /rules/{id}/activate.
type ActivateRequest struct {
	Revision int    `json:"revision"`
	Mode     string `json:"mode"`
	ActedBy  string `json:"acted_by"`
	Reason   string `json:"reason"`
}

func (s *Server) activate(w http.ResponseWriter, r *http.Request) {
	var body ActivateRequest
	if !decode(w, r, &body) {
		return
	}
	rule, err := s.store.Activate(r.PathValue("id"), body.Revision, body.Mode, body.ActedBy, body.Reason)
	if respondError(w, err) {
		return
	}
	// The request's context ends with the response; the check outlives it.
	s.reactor.CheckRuleInBackground(s.lifetime, rule.ID)
	writeJSON(w, http.StatusOK, rule)
}

// StateChangeRequest is the body of pause and retire.
type StateChangeRequest struct {
	ActedBy string `json:"acted_by"`
	Reason  string `json:"reason"`
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	var body StateChangeRequest
	if !decode(w, r, &body) {
		return
	}
	rule, err := s.store.Pause(r.PathValue("id"), body.ActedBy, body.Reason)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (s *Server) retire(w http.ResponseWriter, r *http.Request) {
	var body StateChangeRequest
	if !decode(w, r, &body) {
		return
	}
	rule, err := s.store.Retire(r.PathValue("id"), body.ActedBy, body.Reason)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// PreviewRequest is the body of POST /rules/{id}/preview: a revision number,
// or a definition not yet saved.
type PreviewRequest struct {
	Revision   int             `json:"revision"`
	Definition json.RawMessage `json:"definition"`
	Limit      int             `json:"limit"`
}

// PreviewMatch is one session a definition would match now.
type PreviewMatch struct {
	SubjectID      string          `json:"subject_id"`
	TriggerKey     string          `json:"trigger_key"`
	Facts          map[string]any  `json:"facts"`
	PlannedEffects []PlannedEffect `json:"planned_effects"`
}

// previewMatchLimit is the most matches one preview returns by default.
const previewMatchLimit = 50

// preview checks a definition against every session rule-store holds now and
// records nothing. It is not permission: activating is a separate act.
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	var body PreviewRequest
	if !decode(w, r, &body) {
		return
	}
	rule, err := s.store.GetRule(r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	var definition Definition
	switch {
	case len(body.Definition) > 0 && body.Revision != 0:
		writeErr(w, http.StatusBadRequest, "send revision or definition, not both")
		return
	case len(body.Definition) > 0:
		if definition, err = ParseDefinition(rule.Source, body.Definition); respondError(w, err) {
			return
		}
	default:
		found := false
		for _, rev := range rule.Revisions {
			if rev.Revision == body.Revision {
				definition, found = rev.Definition, true
			}
		}
		if !found {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("rule %s has no revision %d", rule.ID, body.Revision))
			return
		}
	}
	limit := body.Limit
	if limit <= 0 {
		limit = previewMatchLimit
	}
	subjects, err := s.store.SubjectsInStates(SubjectTypeSession, statesNamedBy(definition))
	if respondError(w, err) {
		return
	}
	matches := []PreviewMatch{}
	total := 0
	at := time.Now()
	for _, subject := range subjects {
		decision, err := EvaluateSession(r.Context(), definition, subject, at, s.kanban)
		if respondError(w, err) {
			return
		}
		if !decision.Matched {
			continue
		}
		total++
		if len(matches) < limit {
			matches = append(matches, PreviewMatch{SubjectID: subject.ID, TriggerKey: subject.TriggerKey(), Facts: decision.Facts, PlannedEffects: decision.PlannedEffects})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": len(subjects), "matched": total, "matches": matches})
}

func (s *Server) comparison(w http.ResponseWriter, r *http.Request) {
	since, ok := epochParameter(w, r, "since")
	if !ok {
		return
	}
	report, err := s.store.CompareRule(r.Context(), r.PathValue("id"), since, s.kanban, s.reactor.bridge)
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	since, ok := epochParameter(w, r, "since")
	if !ok {
		return
	}
	query := r.URL.Query()
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("limit must be a positive integer, got %q", raw))
			return
		}
		limit = parsed
	}
	runs, err := s.store.ListRuns(RunFilter{RuleID: query.Get("rule_id"), SubjectID: query.Get("subject_id"), Since: since, Limit: limit})
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) getSubject(w http.ResponseWriter, r *http.Request) {
	subject, err := s.store.GetSubject(r.PathValue("type"), r.PathValue("id"))
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, subject)
}

// listActivity serves the reactor's recent activity, newest first. after is
// the last seq the caller has, so a page polling it gets only what is new.
func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var after uint64
	if raw := query.Get("after"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("after must be a sequence number, got %q", raw))
			return
		}
		after = parsed
	}
	kind := query.Get("kind")
	if kind != "" {
		if err := oneOf(kind, ActivityKinds, "activity kind"); err != nil {
			respondError(w, err)
			return
		}
	}
	limit := 200
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > activityCapacity {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("limit must be 1 to %d, got %q", activityCapacity, raw))
			return
		}
		limit = parsed
	}
	writeJSON(w, http.StatusOK, s.reactor.Activity(after, kind, limit))
}

// RuleSummary is one rule with its run counts, for an overview page.
type RuleSummary struct {
	Rule
	RunsTotal     int   `json:"runs_total"`
	RunsLast24h   int   `json:"runs_last_24h"`
	LastDecidedAt int64 `json:"last_decided_at"`
}

// summary is everything an overview page shows in one read: the reactor's
// status, every rule with its active definition and run counts.
func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.ListRules("")
	if respondError(w, err) {
		return
	}
	counts, err := s.store.RunCountsByRule(time.Now().Add(-24 * time.Hour).Unix())
	if respondError(w, err) {
		return
	}
	summaries := make([]RuleSummary, 0, len(rules))
	for _, listed := range rules {
		full, err := s.store.GetRule(listed.ID)
		if respondError(w, err) {
			return
		}
		c := counts[listed.ID]
		summaries = append(summaries, RuleSummary{Rule: *full, RunsTotal: c.Total, RunsLast24h: c.Recent, LastDecidedAt: c.LastDecidedAt})
	}
	storeCounts, err := s.store.Counts()
	if respondError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": storeCounts, "reactor": s.reactor.Status(), "rules": summaries})
}

func epochParameter(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s must be epoch seconds, got %q", name, raw))
		return 0, false
	}
	return value, true
}

// decode reads a JSON body, refusing unknown fields so a misspelled key is a
// 400 rather than a write that silently drops it.
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func respondError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
	return true
}
