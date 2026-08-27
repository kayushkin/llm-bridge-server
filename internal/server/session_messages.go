package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// The materialized-page response, and the one fact the bridge adds to it.
//
// WHY THIS IS NOT A BARE PROXY. log-store owns the transcript; llm-bridge-server owns
// the event stream and its row ids. A client opening a session needs both, and needs
// them CONSISTENT with each other — so the two facts are composed here, where the
// ordering that makes them consistent can be enforced, rather than left to the client
// to ask for separately and hope nothing moved in between.
//
// What it buys: the SSE handler replays the entire current turn to a client that
// connects without a `Last-Event-ID`, because it has no way to know what the client
// already has. Measured 2026-08-26 on three real sessions, that replay was 88, 177 and
// 360 frames — up to 1,154 KB — every one of which the client had just been sent on the
// page microseconds earlier. Handed a resume point, the same connects replay 0 frames
// and 0 bytes.

// SessionMessagesResponse is `GET /sessions/{id}/messages` and `.../messages/raw`.
//
// `Model` is log-store's materialized TurnModel, passed through as raw bytes. It is
// deliberately NOT decoded: this layer has no opinion about the transcript and parsing
// a megabyte of JSON to re-encode it unchanged would be both slow and a place for the
// two representations to drift.
type SessionMessagesResponse struct {
	Model json.RawMessage `json:"model"`

	// Stream is where to resume the session's event stream so that nothing is missed
	// and nothing already on the page is re-sent.
	Stream StreamResumePoint `json:"stream"`
}

// StreamResumePoint names the point in a session's event stream that the accompanying
// page already covers.
//
// `Head` is an llm-bridge-server event row id — the id space of the SSE `id:` line and
// of `Last-Event-ID`. ⚠️ It is NOT the log-store row id that the page's `entry.eventId`
// carries. The two stores number the same events independently, and comparing across
// them is meaningless; that confusion has already cost this codebase a bug where every
// reconnect silently replayed nothing. Carried as its own field, under its own name, so
// there is no way to reach for the wrong one.
//
// A `Head` of 0 means the session has no stored events, and a client should connect
// without a resume point.
type StreamResumePoint struct {
	Head int64 `json:"head"`
}

// handleSessionMessages serves the materialized page together with the stream resume
// point that page is consistent with.
//
// The ORDER of the three steps below is the correctness of the whole feature, and it is
// the thing to protect in review:
//
//  1. read the stream head
//  2. flush pending log-store writes
//  3. fetch the page
//
// That order guarantees page ⊇ {events ≤ head}: the flush drains everything queued when
// it runs, which includes every event that existed at step 1. The client resumes from
// `head` and receives everything above it, so the union is the whole stream with no gap.
//
// Reversing steps 1 and 2 looks equivalent and loses data: an event written between the
// flush and the read is at or below the head while never having reached log-store, so it
// appears on neither the page nor the resumed stream and nothing reports it missing.
func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// STEP 1 — before the flush. See above.
	head, err := s.store.MaxEventRowID(id)
	if err != nil {
		http.Error(w, fmt.Sprintf("stream head unavailable: %v", err), http.StatusInternalServerError)
		return
	}

	// STEP 2 + 3 — the existing proxy does both: it flushes this session's queued
	// writes and then fetches the page from log-store.
	body, status, err := s.fetchFromLogStore(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store unreachable: %v", err), http.StatusBadGateway)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		w.Write(body)
		return
	}

	// log-store answers `{"model":{…}}`. Only the wrapper is decoded; the model itself
	// rides through as bytes.
	var page struct {
		Model json.RawMessage `json:"model"`
	}
	if err := json.Unmarshal(body, &page); err != nil || page.Model == nil {
		// Not the shape this route expects. Pass log-store's answer through unchanged
		// rather than inventing an envelope around something we did not understand —
		// a client that gets a truthful surprise can report it; one that gets a
		// plausible wrapper around nothing cannot.
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
		return
	}

	writeJSON(w, SessionMessagesResponse{
		Model:  page.Model,
		Stream: StreamResumePoint{Head: head},
	})
}

// fetchFromLogStore flushes this session's queued writes and returns log-store's answer
// for the request's own path and query. Shares its path derivation with
// `proxyToLogStore`, so a route added to one is reachable from the other.
func (s *Server) fetchFromLogStore(r *http.Request) ([]byte, int, error) {
	id := r.PathValue("id")
	endpoint := logStoreEndpointFromPath(r.URL.EscapedPath())
	if endpoint == "" {
		return nil, 0, fmt.Errorf("not a log-store session route: %s", r.URL.EscapedPath())
	}
	s.harness.FlushLogStoreWrites(id)

	target := logStoreSessionURL(s.cfg.LogStoreURL, id, endpoint)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	resp, err := http.Get(target)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}
