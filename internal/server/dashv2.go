package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// handleSessionsSummary serves the projected sidebar list, newest-first and
// cursor-paginated: GET /sessions/summary?limit=100&before=<cursor>.
//
// The response carries a `revision` (max updated_at across the table) that is
// also emitted as the ETag header; an If-None-Match that equals the current
// revision short-circuits to 304. The serialized body is response-cached (L3),
// invalidated by the store Notifier. Everything here is additive — no existing
// endpoint changes.
func (s *Server) handleSessionsSummary(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	before := r.URL.Query().Get("before")

	revision, err := s.store.MaxSessionUpdatedAt()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	etag := etagFor(revision)

	// Conditional GET: a client whose ETag matches the current revision needs
	// nothing. Note the revision is table-wide, so any mutation anywhere busts
	// every page's ETag — correct, if conservative.
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	cacheKey := fmt.Sprintf("summary|rev=%s|limit=%d|before=%s", revision, limit, before)
	if body, ok := s.responseCache.get(cacheKey); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", etag)
		w.Write(body)
		return
	}

	rows, err := s.store.ListSessionSummaries(limit, before)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sessions := make([]SessionSummary, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, summaryFromRow(row))
	}
	var next *string
	// A full page implies there may be more; carry the last row's opaque cursor.
	if len(rows) == limit && limit > 0 {
		c := rows[len(rows)-1].Cursor
		next = &c
	}
	resp := SummaryResponse{Sessions: sessions, Next: next, Revision: revision}
	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.responseCache.put(cacheKey, body)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	w.Write(body)
}

// handleSessionsValidators serves the cheap staleness check:
// GET /sessions/validators?ids=a,b,c → { [id]: Validator }.
//
// Event row ids live in log-store (they are what Entry.eventId references), so
// the validators are sourced there — the single source of truth. bridge-server
// forwards the id set and passes log-store's already-correct { [id]: Validator }
// body straight through (layers are transparent).
func (s *Server) handleSessionsValidators(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query().Get("ids")
	if strings.TrimSpace(ids) == "" {
		writeJSON(w, map[string]Validator{})
		return
	}
	target := fmt.Sprintf("%s/api/v1/sessions/validators?ids=%s", s.cfg.LogStoreURL, ids)
	resp, err := http.Get(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleRecentBundle warms the N most-recent sessions in one round trip:
// GET /sessions/recent-bundle?n=20&turns=30 → { [id]: { summary, model } }.
//
// bridge-server owns the summaries (bridge.db); the materialized turn models
// come from log-store. The two are assembled here into one response and
// response-cached (keyed by the max updated_at across the N), invalidated by the
// store Notifier. The upstream bundle is DECODED STREAMING (json.Decoder over
// the response body) rather than buffered whole then re-marshalled.
func (s *Server) handleRecentBundle(w http.ResponseWriter, r *http.Request) {
	n := 20
	if v := r.URL.Query().Get("n"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	turns := 30
	if v := r.URL.Query().Get("turns"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			turns = x
		}
	}

	rows, err := s.store.ListSessionSummaries(n, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Cache key = the scope's newest updated_at (rows are sorted desc) + shape.
	scopeRev := ""
	if len(rows) > 0 {
		scopeRev = rows[0].Cursor
	}
	cacheKey := fmt.Sprintf("bundle|rev=%s|n=%d|turns=%d|count=%d", scopeRev, n, turns, len(rows))
	if body, ok := s.responseCache.get(cacheKey); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
		return
	}

	if len(rows) == 0 {
		body := []byte("{}")
		s.responseCache.put(cacheKey, body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
		return
	}

	ids := make([]string, 0, len(rows))
	summaries := make(map[string]SessionSummary, len(rows))
	for _, row := range rows {
		ids = append(ids, row.SessionID)
		summaries[row.SessionID] = summaryFromRow(row)
	}

	// Fetch materialized tails from log-store, decoding the response streaming.
	models, err := s.fetchBundleModels(ids, turns)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store bundle: %v", err), http.StatusBadGateway)
		return
	}

	out := make(map[string]bundleEntry, len(rows))
	for _, id := range ids {
		out[id] = bundleEntry{Summary: summaries[id], Model: models[id]}
	}
	body, err := json.Marshal(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.responseCache.put(cacheKey, body)
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// fetchBundleModels calls log-store's bundle endpoint and returns each session's
// raw TurnModel JSON, decoded streaming from the response body (no full-body
// buffer + re-marshal). A missing id maps to a null model.
func (s *Server) fetchBundleModels(ids []string, turns int) (map[string]json.RawMessage, error) {
	target := fmt.Sprintf("%s/api/v1/sessions/bundle?ids=%s&turns=%d",
		s.cfg.LogStoreURL, strings.Join(ids, ","), turns)
	resp, err := http.Get(target)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("log-store returned %d", resp.StatusCode)
	}
	var models map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, err
	}
	return models, nil
}

// etagFor builds a weak-free strong ETag value from the revision string.
func etagFor(revision string) string {
	return `"` + revision + `"`
}
