package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge/msg"
)

// The structured notification producer — the second `source:"tool"` writer,
// beside the parked AskUserQuestion in signals.go.
//
// Until this route existed, every notification in the store came from the
// turn-end classifier: a cheap model reading the assistant's closing text and
// guessing whether it was worth surfacing. That producer is useful and stays,
// but it can only report what the session happened to say last, and only once
// the turn is over. This route lets a session say the thing on purpose, in the
// middle of a turn, in the record's own shape — a headline, a body, and a
// severity it chose rather than one inferred from prose.
//
// The whole verb rests on one property: a notification asks for nothing. It
// parks no tool call, it does not move the session's state, and the session
// keeps running across the call. That is why it is safe for an unattended
// worker to raise one — the failure mode that makes an autonomous
// AskUserQuestion a deny (a worker blocking on a human who is not there)
// cannot happen here.

// Bounds on the text a caller may store. The caller is a language model, so
// these are the difference between a headline and a transcript pasted into a
// card. Both are refusals rather than truncations: a 400 naming the limit is
// something the model reads and retries, while a silent trim puts half a
// message on the user's board and calls it delivered.
const (
	notifyMaxTitleRunes = 200
	notifyMaxBodyRunes  = 4000
)

// notifyRequest is the body of POST /sessions/{id}/signals.
//
// Kind is accepted but constrained — see handleCreateSessionSignal. Source,
// surface, state and linked_todo_id are all stamped by the server; a caller
// that could set them could claim to be the parked-hook producer, or route
// its own notification to a surface nobody watches.
type notifyRequest struct {
	Title    string             `json:"title"`
	Body     string             `json:"body"`
	Severity msg.SignalSeverity `json:"severity"`
	Kind     msg.SignalKind     `json:"kind"`
}

// handleCreateSessionSignal serves POST /sessions/{id}/signals — a session
// raising a notification about itself.
//
// It mints one `source:"tool"`, `kind:"notification"` row and returns it. The
// row carries no request_id, which is not an omission but the contract: there
// is no parked call behind it, so the resolve verb's parked-request guard sees
// nothing to respect and `POST /signals/{id}/resolve` can acknowledge or
// dismiss it the moment a human reads it.
//
// What this route deliberately does NOT do:
//
//   - It does not touch session state. A notification is non-blocking by
//     definition; walking the session to awaiting_user would turn a remark
//     into a park with no answer behind it.
//   - It does not mint questions. See the refusal below.
//   - It does not deduplicate. A session that notifies in a loop writes a row
//     each time, and that is visible in the inbox rather than hidden by a
//     silent cap.
func (s *Server) handleCreateSessionSignal(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req notifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// `kind` is optional and notification is the only value this verb mints.
	// A question is refused rather than quietly downgraded, because a
	// `source:"tool"` question minted here would be answerable by nothing:
	// the hook-resolve verb is keyed on a request_id this row has none of,
	// `POST /sessions/{id}/send` closes derived questions only
	// (closeQuestionsAnsweredByMessage), and the signal-level resolve verb refuses to
	// acknowledge a question on purpose. It would sit open until a human
	// dismissed it unanswered. AskUserQuestion is the verb that asks, and it
	// parks a real call to hang the answer on.
	switch req.Kind {
	case "", msg.SignalKindNotification:
	case msg.SignalKindQuestion:
		http.Error(w, "kind=question cannot be raised here: nothing parks behind it, so no verb could deliver an answer — "+
			"the hook resolve is keyed on a request_id this row would not have, and POST /sessions/"+bridgeID+
			"/send only closes derived questions. Call AskUserQuestion to ask; this route raises notifications.",
			http.StatusBadRequest)
		return
	default:
		http.Error(w, errBadEnum("kind", string(req.Kind), "notification").Error(), http.StatusBadRequest)
		return
	}

	// An unknown severity is a 400 rather than a fallback to info, on the same
	// reasoning as the read filters: a typo that silently grades a warning as
	// routine is worse than a rejected call the caller can fix.
	severity := msg.SignalSeverityInfo
	switch req.Severity {
	case "", msg.SignalSeverityInfo:
	case msg.SignalSeverityWarn:
		severity = msg.SignalSeverityWarn
	default:
		http.Error(w, errBadEnum("severity", string(req.Severity), "info|warn").Error(), http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)
	if title == "" {
		http.Error(w, "title is required: it is the headline the human reads, and a row without one renders blank",
			http.StatusBadRequest)
		return
	}
	if n := len([]rune(title)); n > notifyMaxTitleRunes {
		http.Error(w, fmt.Sprintf("title is %d characters and the limit is %d; put the detail in body", n, notifyMaxTitleRunes),
			http.StatusBadRequest)
		return
	}
	if n := len([]rune(body)); n > notifyMaxBodyRunes {
		http.Error(w, fmt.Sprintf("body is %d characters and the limit is %d", n, notifyMaxBodyRunes),
			http.StatusBadRequest)
		return
	}

	signal := &msg.Signal{
		ID:          ids.NewSignalID(),
		SessionID:   bridgeID,
		SessionType: sess.Type,
		Kind:        msg.SignalKindNotification,
		Source:      msg.SignalSourceTool,
		// Surface comes from the session, never from the caller: an
		// autonomous worker's notification belongs on its kanban card whether
		// or not the worker knows it has one.
		Surface:      signalSurfaceForSession(sess),
		Title:        title,
		Body:         body,
		Severity:     severity,
		State:        msg.SignalStateOpen,
		LinkedTodoID: s.linkedTodoForSession(bridgeID),
	}

	// Unlike recordAskUserQuestionSignals, a failure here is reported. That
	// one is observational — it rides along beside a park that must happen
	// whatever the signal table does — while this call IS the request, and a
	// caller told 200 would believe a notification reached the human.
	if err := s.store.CreateSignal(signal); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Re-read rather than echo, for the same reason the resolve verb does:
	// the response is what a later GET will return, including the fields the
	// store filled in, not a hopeful copy of what was sent.
	stored, err := s.store.GetSignal(signal.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[signals] %s: tool notification %s (severity=%s surface=%s)",
		bridgeID, stored.ID, stored.Severity, stored.Surface)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(stored)
}
