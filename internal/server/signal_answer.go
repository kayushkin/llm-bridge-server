package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// signalAnswerRequest carries the answers for one question group.
//
// Keyed by SIGNAL ID, which is what a form holds. The title-keyed payload the
// parked hook pairs on is derived server-side, so no caller has to know that
// the tool path matches answers by question text.
type signalAnswerRequest struct {
	Answers    map[string]string `json:"answers"`
	ResolvedBy string            `json:"resolved_by,omitempty"`
}

// handleAnswerSignal serves POST /signals/{id}/answer — the ONE way to answer
// a question, whichever producer raised it and whether or not its session is
// still running.
//
// This is the seam that used to live in the browser. chat-core inspected
// request_id, then either (a) re-read GET /sessions/{id}/hooks/pending, merged
// the answers into the parked tool input, and POSTed it to the hook resolve
// route, or (b) POSTed the text to /sessions/{id}/send. Two transports, chosen
// by a client, over a distinction only the server can actually evaluate: a
// request_id says a park EXISTED, not that it is still live, and only the
// server knows which. Every surface that wanted to answer a question had to
// reimplement all of it.
//
// The choice is now made here, once, and it is a choice about DELIVERY, not
// about the question:
//
//   - the park is still live      → hand the answer to the blocked tool call
//   - the park is gone, or there
//     never was one (derived)     → deliver it as the session's next message,
//     starting the process again if the
//     session has since been reaped
//
// Both close the same rows the same way. A caller cannot tell the two apart
// except by the informational delivered_via in the response, and must not try.
func (s *Server) handleAnswerSignal(w http.ResponseWriter, r *http.Request) {
	signalID := r.PathValue("id")
	signal, err := s.store.GetSignal(signalID)
	if err != nil {
		http.Error(w, "signal not found", http.StatusNotFound)
		return
	}
	if signal.Kind != msg.SignalKindQuestion {
		writeJSONError(w, http.StatusBadRequest, "not_a_question",
			fmt.Sprintf("signal %s is a %s; only a question can be answered (acknowledge or dismiss a notification)", signalID, signal.Kind))
		return
	}
	if signal.State != msg.SignalStateOpen {
		writeJSONError(w, http.StatusConflict, "already_resolved",
			fmt.Sprintf("signal %s is already %s", signalID, signal.State))
		return
	}

	sess, err := s.store.GetSession(signal.SessionID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req signalAnswerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	group, err := s.openQuestionGroup(signal)
	if err != nil {
		http.Error(w, fmt.Sprintf("look up the question group: %v", err), http.StatusInternalServerError)
		return
	}

	// One AskUserQuestion call carries several questions and resolves ONCE, so
	// a partial answer would resolve the whole request with the rest blank.
	// Enforced here rather than in a card, because the card is not the only
	// caller and the invariant belongs to the request.
	missing := make([]string, 0, len(group))
	for _, q := range group {
		if req.Answers[q.ID] == "" {
			missing = append(missing, q.Title)
		}
	}
	if len(missing) > 0 {
		writeJSONError(w, http.StatusBadRequest, "incomplete_answer",
			fmt.Sprintf("this request carries %d questions and resolves once; unanswered: %v", len(group), missing))
		return
	}

	if signal.RequestID != "" && s.parkedAsks.isParked(signal.SessionID, signal.RequestID) {
		if s.answerThroughParkedHook(signal, group, req) {
			writeJSON(w, map[string]any{"status": "answered", "delivered_via": "parked_hook"})
			return
		}
		// The park drained between isParked and deliver. Not an error — it is
		// the ordinary race, and the message path is the correct fallback
		// rather than a failure to report.
		log.Printf("[signals] %s/%s: park vanished mid-answer; delivering as a message instead", signal.SessionID, signal.RequestID)
	}

	if err := s.answerAsNextMessage(r, sess, group, req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "answered", "delivered_via": "message"})
}

// openQuestionGroup returns every open question that resolves together with
// this one: the whole parked request, or the single row a derived question is.
func (s *Server) openQuestionGroup(signal *store.Signal) ([]store.Signal, error) {
	if signal.RequestID == "" {
		return []store.Signal{*signal}, nil
	}
	siblings, err := s.store.ListSignalsByRequestID(signal.SessionID, signal.RequestID)
	if err != nil {
		return nil, err
	}
	group := make([]store.Signal, 0, len(siblings))
	for _, sib := range siblings {
		if sib.State == msg.SignalStateOpen && sib.Kind == msg.SignalKindQuestion {
			group = append(group, sib)
		}
	}
	return group, nil
}

// answerThroughParkedHook hands the answers to the tool call still blocked on
// them. Reports whether the parked entry was still there to take them.
//
// The parked tool input is fetched and passed back UNDER the answers because
// the hook resolve replaces the tool input wholesale — rebuilding it from the
// signal rows would drop whatever the record does not carry (multiSelect,
// option previews). The client used to do this fetch itself; it is server-side
// now, which also removes the window where a client held a stale copy.
func (s *Server) answerThroughParkedHook(signal *store.Signal, group []store.Signal, req signalAnswerRequest) bool {
	// The hook pairs answers by question TEXT, which is what the row's title
	// was minted from. That mapping is this function's business alone.
	answersByTitle := make(map[string]string, len(group))
	for _, q := range group {
		answersByTitle[q.Title] = req.Answers[q.ID]
	}

	updated := s.parkedToolInputWithAnswers(signal.SessionID, signal.RequestID, answersByTitle)
	decision := permissionDecision{
		Behavior:     "allow",
		UpdatedInput: updated,
		ResolvedBy:   resolvedByOrUser(req.ResolvedBy),
	}
	if !s.parkedAsks.deliver(signal.SessionID, signal.RequestID, decision) {
		return false
	}
	// The prehook handler that was blocked on the channel now wakes and
	// broadcasts the resolution, which closes the rows through
	// resolveSignalsForRequest. Closing them here as well would race it.
	return true
}

// parkedToolInputWithAnswers merges answers into the live parked tool input.
// Falls back to an answers-only payload when the pending hook cannot be found,
// which is the shape the hook accepts anyway — a caller has still answered,
// and refusing over a missing preview would fail a question that works.
func (s *Server) parkedToolInputWithAnswers(bridgeID, requestID string, answersByTitle map[string]string) json.RawMessage {
	merged := map[string]any{}
	for _, ev := range s.harness.PendingHooks(bridgeID) {
		if ev.Hook == nil || ev.Hook.RequestID != requestID || len(ev.Hook.Input) == 0 {
			continue
		}
		if err := json.Unmarshal(ev.Hook.Input, &merged); err != nil {
			log.Printf("[signals] %s/%s: parked tool input unparseable, answering without it: %v", bridgeID, requestID, err)
			merged = map[string]any{}
		}
		break
	}
	merged["answers"] = answersByTitle
	encoded, err := json.Marshal(merged)
	if err != nil {
		log.Printf("[signals] %s/%s: re-encode tool input: %v", bridgeID, requestID, err)
		return nil
	}
	return encoded
}

// answerAsNextMessage delivers the answer as the session's next turn, which is
// how a question is answered when nothing is waiting on a channel: a derived
// question never parked anything, and a parked one whose process has since
// died has nothing left to deliver to.
//
// Starting a stopped session is the point, not a side effect. A question
// outlives its session, so answering one is exactly the moment to bring that
// session back — the person answering is continuing the conversation and
// wants to see what happens next.
func (s *Server) answerAsNextMessage(r *http.Request, sess *store.Session, group []store.Signal, req signalAnswerRequest) error {
	text := answerTextForMessage(group, req.Answers)

	if s.harness.Get(sess.SessionID) == nil {
		if err := s.restartForAnswer(r, sess); err != nil {
			return err
		}
	}

	userEvent := msg.Event{
		Type:            msg.EventUserMessage,
		BridgeSessionID: sess.SessionID,
		Timestamp:       time.Now(),
		Result:          &msg.ResultEvent{Text: text},
	}
	if _, err := s.harness.BroadcastEvent(&userEvent); err != nil {
		return fmt.Errorf("persist user_message: %w", err)
	}
	if err := s.harness.Send(sess.SessionID, text, nil); err != nil {
		return err
	}

	// Answering an archived session reopens it, for the same reason replying
	// to one does: you are continuing it, and you are about to want to see the
	// result. State stays with the derivation pipeline; only the folder moves.
	if sess.FolderName == store.ArchiveFolder {
		if err := s.store.SetSessionFolder(sess.SessionID, ""); err != nil {
			log.Printf("[signals] failed to reopen archived session %s on answer: %v", sess.SessionID, err)
		}
	}

	for _, q := range group {
		answer := &msg.SignalAnswer{Text: req.Answers[q.ID]}
		if err := s.store.ResolveSignal(q.ID, msg.SignalStateAnswered, answer); err != nil {
			log.Printf("[signals] %s: resolve on answer: %v", q.ID, err)
		}
	}
	return nil
}

// restartForAnswer brings back the process a reaped session lost.
func (s *Server) restartForAnswer(r *http.Request, sess *store.Session) error {
	if sess.InstanceID == "" || s.harnessStore == nil {
		return fmt.Errorf("session has no instance bound, so its answer cannot be delivered")
	}
	inst, err := s.harnessStore.GetInstance(sess.InstanceID)
	if err != nil {
		return fmt.Errorf("instance not found: %w", err)
	}
	credID := resolveCredential(s.harnessStore, inst.ID)
	if _, err := s.startOnInstance(r.Context(), sess, inst, credID); err != nil {
		return fmt.Errorf("failed to start harness: %w", err)
	}
	return nil
}

// answerTextForMessage renders a group as the one message that carries it.
// A single question sends its answer verbatim — anything else would put words
// in the user's mouth. Several questions are labelled, because the assistant
// asked them separately and an unlabelled concatenation is unreadable.
func answerTextForMessage(group []store.Signal, answers map[string]string) string {
	if len(group) == 1 {
		return answers[group[0].ID]
	}
	out := ""
	for i, q := range group {
		if i > 0 {
			out += "\n\n"
		}
		out += q.Title + "\n" + answers[q.ID]
	}
	return out
}

func resolvedByOrUser(resolvedBy string) string {
	if resolvedBy == "" {
		return "user"
	}
	return resolvedBy
}
