package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// signalSurfaceForSession reports where a signal raised by this session
// renders. Attended sessions (interactive, herald, system) route to chat —
// somebody is reading it. Autonomous workers route to their kanban card,
// because nothing routes to a chat nobody has open.
//
// This deliberately differs from isUnattendedSession, which groups herald
// WITH autonomous. The two answer different questions: isUnattendedSession
// asks "can a parked tool ask ever be resolved on this turn?" (no for
// herald — the relay turn itself has no human on it), while surface asks
// "where does the human eventually read this?" (the chat inbox, for a
// herald relay). Collapsing them would send herald signals to a kanban card
// no herald session has.
func signalSurfaceForSession(sess *store.Session) msg.SignalSurface {
	if sess != nil && sess.Type == msg.SessionTypeAutonomous {
		return msg.SignalSurfaceKanban
	}
	return msg.SignalSurfaceChat
}

// askUserQuestionInput is the tool input Claude Code sends for
// AskUserQuestion. Mirrors the shape bridge-ui already renders in
// PendingPermissionsBanner.
type askUserQuestionInput struct {
	Questions []askUserQuestionQuestion `json:"questions"`
}

type askUserQuestionQuestion struct {
	Question    string                  `json:"question"`
	Header      string                  `json:"header"`
	Options     []askUserQuestionOption `json:"options"`
	MultiSelect bool                    `json:"multiSelect"`
}

type askUserQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// askUserQuestionResolution is the updated_input bridge-ui posts back when
// the human answers: the original questions plus an answers map keyed by
// question text. Multi-select answers arrive as one comma-joined string,
// which is what Claude Code's own input schema accepts.
type askUserQuestionResolution struct {
	Answers map[string]string `json:"answers"`
}

// recordAskUserQuestionSignals writes one signal row per question in a
// parked AskUserQuestion request. One tool call carries an array of
// questions, and the record holds a single title and option set, so the
// honest mapping is one signal per question rather than a flattened
// summary. They share RequestID because the resolve verb is per-request:
// answering the parked hook answers all of them at once.
//
// Recording is observational — a failure here must not block the park, so
// errors are logged and swallowed.
func (s *Server) recordAskUserQuestionSignals(bridgeID string, sess *store.Session, requestID string, toolInput json.RawMessage) {
	var input askUserQuestionInput
	if err := json.Unmarshal(toolInput, &input); err != nil {
		log.Printf("[signals] %s/%s: AskUserQuestion input unparseable, no signal recorded: %v", bridgeID, requestID, err)
		return
	}
	if len(input.Questions) == 0 {
		log.Printf("[signals] %s/%s: AskUserQuestion carried no questions, no signal recorded", bridgeID, requestID)
		return
	}

	sessionType := msg.SessionType("")
	if sess != nil {
		sessionType = sess.Type
	}
	surface := signalSurfaceForSession(sess)

	for _, question := range input.Questions {
		options := make([]msg.SignalOption, 0, len(question.Options))
		for _, opt := range question.Options {
			options = append(options, msg.SignalOption{
				// AskUserQuestion has no machine value distinct from the
				// label — the label IS what the resolve verb sends back.
				Label:       opt.Label,
				Value:       opt.Label,
				Description: opt.Description,
			})
		}
		signal := &msg.Signal{
			ID:          ids.NewSignalID(),
			SessionID:   bridgeID,
			SessionType: sessionType,
			Kind:        msg.SignalKindQuestion,
			Source:      msg.SignalSourceTool,
			RequestID:   requestID,
			Surface:     surface,
			Title:       question.Question,
			Body:        question.Header,
			Options:     options,
			// The resolve verb takes an arbitrary answer string, so a typed
			// answer is accepted whether or not the model offered options.
			AllowFreeform: true,
			State:         msg.SignalStateOpen,
		}
		if err := s.store.CreateSignal(signal); err != nil {
			log.Printf("[signals] %s/%s: persist signal: %v", bridgeID, requestID, err)
		}
	}
}

// resolveSignalsForRequest closes the signals minted from a parked hook
// request once that request is decided. An allow carries the human's
// answers in updated_input, keyed by question text, which is exactly the
// title each signal was minted with; a deny (or a canceled park) resolves
// them as dismissed with no answer.
//
// No-op when the request minted no signals, which covers every
// permission-prompt request and any pre-existing parked ask from before
// this table existed.
func (s *Server) resolveSignalsForRequest(bridgeID, requestID string, decision permissionDecision) {
	if requestID == "" {
		return
	}
	signals, err := s.store.ListSignalsByRequestID(bridgeID, requestID)
	if err != nil {
		log.Printf("[signals] %s/%s: look up signals to resolve: %v", bridgeID, requestID, err)
		return
	}
	if len(signals) == 0 {
		return
	}

	answers := map[string]string{}
	if decision.Behavior == "allow" && len(decision.UpdatedInput) > 0 {
		var resolution askUserQuestionResolution
		if err := json.Unmarshal(decision.UpdatedInput, &resolution); err != nil {
			log.Printf("[signals] %s/%s: updated_input unparseable, resolving without answers: %v", bridgeID, requestID, err)
		} else if resolution.Answers != nil {
			answers = resolution.Answers
		}
	}

	for _, signal := range signals {
		if signal.State != msg.SignalStateOpen {
			continue
		}
		state := msg.SignalStateDismissed
		var answer *msg.SignalAnswer
		if decision.Behavior == "allow" {
			state = msg.SignalStateAnswered
			if text, ok := answers[signal.Title]; ok {
				answer = &msg.SignalAnswer{Text: text}
			}
		}
		if err := s.store.ResolveSignal(signal.ID, state, answer); err != nil {
			log.Printf("[signals] %s/%s: resolve signal %s: %v", bridgeID, requestID, signal.ID, err)
		}
	}
}

// handleListSessionSignals serves GET /sessions/{id}/signals — every signal
// this session has raised, newest first. Optional filters: state, kind,
// surface, limit.
func (s *Server) handleListSessionSignals(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	if _, err := s.store.GetSession(bridgeID); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	filter, err := signalFilterFromQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter.SessionID = bridgeID
	signals, err := s.store.ListSignals(filter)
	writeSignals(w, signals, err)
}

// handleListSignals serves GET /signals — the cross-session inbox query.
// Same optional filters as the per-session route; `?state=open` is the one
// the "Needs you" inbox uses.
func (s *Server) handleListSignals(w http.ResponseWriter, r *http.Request) {
	filter, err := signalFilterFromQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter.SessionID = r.URL.Query().Get("session_id")
	signals, err := s.store.ListSignals(filter)
	writeSignals(w, signals, err)
}

func writeSignals(w http.ResponseWriter, signals []store.Signal, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if signals == nil {
		signals = []store.Signal{}
	}
	writeJSON(w, signals)
}

// signalFilterFromQuery reads the shared filter params. Unknown enum values
// are rejected rather than silently matching nothing, so a typo in a query
// is a 400 instead of an empty list that reads like "no signals".
func signalFilterFromQuery(r *http.Request) (store.SignalFilter, error) {
	q := r.URL.Query()
	var filter store.SignalFilter

	if v := q.Get("state"); v != "" {
		switch msg.SignalState(v) {
		case msg.SignalStateOpen, msg.SignalStateAnswered, msg.SignalStateAcknowledged, msg.SignalStateDismissed:
			filter.State = msg.SignalState(v)
		default:
			return filter, errBadEnum("state", v, "open|answered|acknowledged|dismissed")
		}
	}
	if v := q.Get("kind"); v != "" {
		switch msg.SignalKind(v) {
		case msg.SignalKindQuestion, msg.SignalKindNotification:
			filter.Kind = msg.SignalKind(v)
		default:
			return filter, errBadEnum("kind", v, "question|notification")
		}
	}
	if v := q.Get("surface"); v != "" {
		switch msg.SignalSurface(v) {
		case msg.SignalSurfaceChat, msg.SignalSurfaceKanban:
			filter.Surface = msg.SignalSurface(v)
		default:
			return filter, errBadEnum("surface", v, "chat|kanban")
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return filter, errBadEnum("limit", v, "a non-negative integer")
		}
		filter.Limit = n
	}
	return filter, nil
}

func errBadEnum(param, got, allowed string) error {
	return fmt.Errorf("%s=%s is not valid; expected %s", param, got, allowed)
}
