package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// handleSessionsSummary serves the projected sidebar list, newest-first and
// cursor-paginated:
// GET /sessions/summary?limit=100&before=<cursor>&type=interactive&type=herald.
//
// The six filter axes (`harness`, `status`, `type`, `purpose`, `mode`,
// `machine`) are the sidebar's own, spelled the same way, and each is REPEATED
// rather than comma-separated: a purpose on this box can read
// "dashv2 browser verification + A/B perf", and nothing stops one containing a
// comma, so splitting on one would silently cut a value in half. Omitting an
// axis filters nothing on it, which keeps every existing caller unchanged.
//
// The response carries a `revision` (max updated_at across the table) that is
// also emitted as the ETag header; an If-None-Match that equals the current
// revision short-circuits to 304. The revision is table-wide, so it is unaffected
// by the filter — conservative, never wrong. The serialized body is
// response-cached (L3), invalidated by the store Notifier.
func (s *Server) handleSessionsSummary(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := 100
	if l := query.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	before := query.Get("before")
	filter := store.SessionSummaryFilter{
		Harnesses:   query["harness"],
		States:      query["status"],
		Types:       query["type"],
		Purposes:    query["purpose"],
		Modes:       query["mode"],
		InstanceIDs: query["machine"],
	}
	sessionIDs, err := summaryIDLookupFromQuery(query, "session_id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter.SessionIDs = sessionIDs

	managerSessionIDs, err := summaryIDLookupFromQuery(query, "manager_session_id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter.ManagerSessionIDs = managerSessionIDs

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

	// The filter is part of the key, not decoration beside it. Two requests that
	// differ only by filter share a revision, a limit and a cursor, so without
	// this the first one to land would have its page served to the other — a
	// wrong answer indistinguishable from a right one.
	cacheKey := fmt.Sprintf("summary|rev=%s|limit=%d|before=%s|filter=%s",
		revision, limit, before, filter.CacheKey())
	if body, ok := s.responseCache.get(cacheKey); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", etag)
		w.Write(body)
		return
	}

	rows, err := s.store.ListSessionSummaries(limit, before, filter)
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

	// Unfiltered on purpose: the recent-bundle warms whatever the user is most
	// likely to open next, and narrowing it to the sidebar's current chips would
	// leave a session cold the moment they cleared a filter.
	rows, err := s.store.ListSessionSummaries(n, "", store.SessionSummaryFilter{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Cache key = the scope's newest row's cursor (rows are sorted desc) + shape.
	// The cursor is updated_at plus that row's session id, so it moves whenever
	// the newest row's timestamp OR identity changes — strictly finer-grained
	// than the timestamp alone, and never coarser.
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

// summaryIDLookupFromQuery reads one repeated id parameter off a summary
// request. Repeatable (?session_id=a&session_id=b) and comma-separated
// (?session_id=a,b) both work, because a caller assembling a list of ids from
// somewhere else should not have to care which shape this endpoint prefers.
//
// Both callers are LOOKUPS, not filter chips — see SessionSummaryFilter, which
// keeps them off axes() for that reason:
//
//   - `session_id` answers "what are these sessions called?" for a caller that
//     already holds the ids. dashv2's signals inbox is the first — it lists open
//     signals across every session, and on this host 11 of the 17 sessions
//     holding one are nowhere near the sidebar's first page, so their cards
//     would otherwise be headed by a raw br_1786635575897138112.
//   - `manager_session_id` answers "what did these sessions spawn?". A parent's
//     children are ordered by their own recency rather than the parent's, so
//     they are unreachable by paging toward the parent.
//
// PRESENT BUT EMPTY IS A 400, not "don't narrow". `?session_id=` with nothing
// after it comes from a caller that meant to name sessions and assembled an
// empty list — most likely from an inbox holding no signals. Treating that as
// an unfiltered request would answer it with the newest hundred sessions on the
// box, and the caller would render every one of them as something waiting on a
// human. The signals endpoint refuses `linked_todo_id=` for exactly this reason;
// this is the same trap on the same shape of parameter.
//
// Blank entries WITHIN a list are dropped rather than refused (`a,,b` is a and
// b): a trailing comma is a formatting slip, not a caller asking for everything,
// and it cannot widen the result — an empty id matches no row.
func summaryIDLookupFromQuery(query url.Values, parameter string) ([]string, error) {
	raw, ok := query[parameter]
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			if id := strings.TrimSpace(part); id != "" {
				out = append(out, id)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s was given but named no session; omit it to list every session", parameter)
	}
	return out, nil
}
