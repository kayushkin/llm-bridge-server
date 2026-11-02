package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The session brief routes: read one session's brief, read many, and answer
// or dismiss a loose end. The brief pass (session_brief_pass.go) is the only
// writer of the brief itself.

// handleGetSessionBrief serves GET /sessions/{id}/brief: the brief, its
// revisions and every loose end, open and closed. A chat no turn has briefed
// yet answers with "brief": null.
func (s *Server) handleGetSessionBrief(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if _, err := s.store.GetSession(sessionID); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	view, err := s.store.GetSessionBriefView(sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

// handleListSessionBriefs serves GET /session-briefs: the briefs of the
// caller's sessions, most recently updated first.
//
//	session_id      repeatable; only these sessions
//	loose_end_state only loose ends in this state beside each brief
//	updated_since   RFC 3339; only briefs updated since
//	limit           at most this many briefs
func (s *Server) handleListSessionBriefs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := store.SessionBriefFilter{SessionIDs: query["session_id"]}
	if state := query.Get("loose_end_state"); state != "" {
		if !slices.Contains(msg.LooseEndStates, msg.LooseEndState(state)) {
			writeJSONError(w, http.StatusBadRequest, "invalid_loose_end_state",
				fmt.Sprintf("loose_end_state %q is not one of %v", state, msg.LooseEndStates))
			return
		}
		filter.LooseEndState = msg.LooseEndState(state)
	}
	if since := query.Get("updated_since"); since != "" {
		parsed, err := time.Parse(time.RFC3339, since)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_updated_since", "updated_since must be an RFC 3339 time: "+err.Error())
			return
		}
		filter.UpdatedSince = parsed
	}
	if limit := query.Get("limit"); limit != "" {
		parsed, err := strconv.Atoi(limit)
		if err != nil || parsed < 1 {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive whole number")
			return
		}
		filter.Limit = parsed
	}
	filter.OwnedByPrincipalID, filter.OwnedThroughPeerPrincipalID = sessionScopeFilterOfRequest(r)
	views, err := s.store.ListSessionBriefs(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, views)
}

// looseEndOfSession reads the loose end a path names and checks it belongs to
// the session the path names; the route's rule has already checked the
// caller may act on that session. Writes the error and returns nil otherwise.
func (s *Server) looseEndOfSession(w http.ResponseWriter, r *http.Request) *msg.LooseEnd {
	looseEnd, err := s.store.GetLooseEnd(r.PathValue("loose_end_id"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && looseEnd.SessionID != r.PathValue("id")) {
		http.Error(w, "loose end not found", http.StatusNotFound)
		return nil
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	if looseEnd.State != msg.LooseEndOpen {
		writeJSONError(w, http.StatusConflict, "loose_end_not_open", fmt.Sprintf("loose end %s is already %s", looseEnd.ID, looseEnd.State))
		return nil
	}
	return looseEnd
}

// looseEndAnswerRequest is the body of POST /sessions/{id}/loose-ends/{loose_end_id}/answer.
type looseEndAnswerRequest struct {
	Text string `json:"text"`
}

// looseEndAnswerMessage is what the session receives: the remark quoted, so
// the assistant knows which of its earlier words the answer is about, and the
// person's answer.
func looseEndAnswerMessage(looseEnd *msg.LooseEnd, answer string) string {
	return fmt.Sprintf("About something you said earlier: \"%s\"\n\n%s", looseEnd.Text, answer)
}

// handleAnswerLooseEnd serves POST /sessions/{id}/loose-ends/{loose_end_id}/answer:
// sends the answer to the session as a message, exactly as if the person typed
// it, and closes the loose end with it.
func (s *Server) handleAnswerLooseEnd(w http.ResponseWriter, r *http.Request) {
	var request looseEndAnswerRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	answer := strings.TrimSpace(request.Text)
	if answer == "" {
		writeJSONError(w, http.StatusBadRequest, "answer_required", "text is the answer and may not be empty")
		return
	}
	looseEnd := s.looseEndOfSession(w, r)
	if looseEnd == nil {
		return
	}
	status, body := s.serveInternally(http.MethodPost, "/sessions/"+url.PathEscape(looseEnd.SessionID)+"/send",
		SendMessageRequest{Message: looseEndAnswerMessage(looseEnd, answer)})
	if status < 200 || status >= 300 {
		writeJSONError(w, http.StatusBadGateway, "send_failed",
			fmt.Sprintf("sending the answer to the session answered %d: %s", status, strings.TrimSpace(string(body))))
		return
	}
	if err := s.store.CloseLooseEnd(looseEnd.ID, msg.LooseEndAnswered, answer); err != nil {
		// The message went; only the record of it failed. Say so rather than
		// report a clean answer.
		http.Error(w, "the answer was sent, but closing the loose end failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "answered"})
}

// handleDismissLooseEnd serves POST /sessions/{id}/loose-ends/{loose_end_id}/dismiss.
func (s *Server) handleDismissLooseEnd(w http.ResponseWriter, r *http.Request) {
	looseEnd := s.looseEndOfSession(w, r)
	if looseEnd == nil {
		return
	}
	if err := s.store.CloseLooseEnd(looseEnd.ID, msg.LooseEndDismissed, ""); err != nil {
		if errors.Is(err, store.ErrLooseEndNotOpen) {
			writeJSONError(w, http.StatusConflict, "loose_end_not_open", err.Error())
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "dismissed"})
}
