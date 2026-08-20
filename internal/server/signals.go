package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

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
	// One lookup for the whole request: every question in it belongs to the
	// same session, so they all propagate to the same todo.
	linkedTodoID := s.linkedTodoForSession(bridgeID)

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
			// Carried onto the record because the record is what the answer
			// form is drawn from. Dropping it here would silently turn a
			// "pick any that apply" into "pick one".
			AllowMultipleOptions: question.MultiSelect,
			State:                msg.SignalStateOpen,
			LinkedTodoID:         linkedTodoID,
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

	// A question outlives the session that asked it.
	//
	// When the process dies mid-park the handler synthesises a deny so Claude
	// Code's blocked tool call gets an answer. That deny is addressed to the
	// TOOL, not to the question: nobody decided anything. Closing the rows
	// here — which is what this function used to do, because a deny is a deny
	// — threw the question away at exactly the moment it became impossible to
	// deliver any other way, and the user never learned it had been asked.
	//
	// Left open, the question is still answerable: the unified answer verb
	// finds no live park and injects the answer as the session's next message,
	// restarting it if needed. The tool call is lost either way — the process
	// it belonged to is gone — but the question survives to be answered.
	if decision.SessionWentAway {
		log.Printf("[signals] %s/%s: session went away mid-park; leaving %d question(s) open to answer later", bridgeID, requestID, len(signals))
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

// signalResolveRequest is the body of POST /signals/{id}/resolve.
type signalResolveRequest struct {
	State msg.SignalState `json:"state"`
}

// resolutionUnparksSession reports whether closing this signal this way also
// has to walk the raising session back to idle.
//
// Only one row type parks its own session: a derived question, which sets
// awaiting_user when the classifier mints it (signal_classifier.go). Dismiss
// it and no answer is coming, so leaving the session at awaiting_user would
// block it with nothing left on screen that explains why. Every other case
// is already consistent — a derived notification drove the session to idle
// at mint time, and a tool signal's session state belongs to its parked
// hook, which this verb refuses to close behind.
//
// A signal that was already closed unparks nothing. Without that the second
// of two duplicate clicks would write session state again — and by then the
// session may be back at awaiting_user on a NEW question, which the
// allowedFrom bound cannot tell apart from the old one. The state update
// itself is a no-op on a closed row, so this is the one place the duplicate
// actually costs something.
//
// A pure function so the cross-product is testable without a live harness:
// the state write itself is bounded and covered in the derivation package.
func resolutionUnparksSession(signal *store.Signal, state msg.SignalState) bool {
	return signal.State == msg.SignalStateOpen &&
		state == msg.SignalStateDismissed &&
		signal.Source == msg.SignalSourceDerived &&
		signal.Kind == msg.SignalKindQuestion
}

// handleResolveSignal serves POST /signals/{id}/resolve — the signal-level
// close verb, and the one thing that lets a surface offer an acknowledge
// button. Until it existed the only way out of `open` was the hook-resolve
// path (tool questions) or a `/send` (derived questions), so a notification
// stayed open forever and `SignalCard`'s onAcknowledge prop was passed by
// nobody.
//
// It closes a signal WITHOUT delivering anything to the raising session,
// which is why it takes only the two states that mean exactly that:
//
//   - acknowledged — a notification was seen. Notifications only: a question
//     that nobody answered has not been handled, and letting it grade the
//     same as a read notification is the enum collapse
//     feedback_status_enum_granularity warns about.
//   - dismissed — closed without an answer. Either kind.
//
// `answered` is refused. An answer has to reach the session, and the two
// paths that carry one already close their own rows: the parked hook resolve
// for a tool question, `POST /sessions/{id}/send` for a derived one. Minting
// an `answered` row here would record an answer the session never received.
func (s *Server) handleResolveSignal(w http.ResponseWriter, r *http.Request) {
	signalID := r.PathValue("id")
	signal, err := s.store.GetSignal(signalID)
	if err != nil {
		http.Error(w, "signal not found", http.StatusNotFound)
		return
	}

	var req signalResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	switch req.State {
	case msg.SignalStateAcknowledged, msg.SignalStateDismissed:
	case msg.SignalStateAnswered:
		// Names ONE route. It used to name two — the hook resolve for a tool
		// question, /send for a derived one — and told the caller to pick,
		// which is the discrimination POST /signals/{id}/answer exists to end.
		// A caller cannot make that choice: a request_id says a park EXISTED,
		// not that it is still live, and only the server knows which.
		http.Error(w, "state=answered is not resolvable here; an answer must reach the session — "+
			"POST /signals/{id}/answer, which delivers it whichever producer raised "+
			"the question and closes the row itself.",
			http.StatusBadRequest)
		return
	default:
		http.Error(w, errBadEnum("state", string(req.State), "acknowledged|dismissed").Error(), http.StatusBadRequest)
		return
	}
	if req.State == msg.SignalStateAcknowledged && signal.Kind == msg.SignalKindQuestion {
		http.Error(w, "a question cannot be acknowledged; answer it, or resolve it with state=dismissed",
			http.StatusBadRequest)
		return
	}

	// Dismissing a question whose park is still LIVE has to deny the parked
	// tool call, not just close the row. The harness is blocked on that hook;
	// closing the surface alone would hide the ask while the process kept
	// waiting on it forever.
	//
	// This used to refuse with a 409 naming the hook route, which pushed the
	// choice back to the caller — and the caller cannot make it, because a
	// request_id says a park EXISTED, not that it is still live. Every surface
	// then had to render two buttons, Decline and Dismiss, and pick between
	// them on that same bad evidence. One verb, and the server picks: the deny
	// closes these rows on its way through resolveSignalsForRequest.
	if req.State == msg.SignalStateDismissed && signal.RequestID != "" &&
		s.parkedAsks.isParked(signal.SessionID, signal.RequestID) {
		decision := permissionDecision{
			Behavior:   "deny",
			Message:    "declined by the user",
			ResolvedBy: "user",
		}
		if s.parkedAsks.deliver(signal.SessionID, signal.RequestID, decision) {
			writeJSON(w, map[string]any{
				"status":        string(msg.SignalStateDismissed),
				"delivered_via": "parked_hook",
			})
			return
		}
		// The park drained between the check and the deliver. Fall through and
		// close the row directly — the ordinary race, not a failure.
	}

	// A second resolve is not an error. The same row renders in the chat, the
	// inbox, the RefChip panel and the kanban drawer, so duplicate clicks are
	// ordinary — and store.ResolveSignal already makes the first resolution
	// the one that happened, updating only while the row is still open. This
	// handler does not repeat that rule; it re-reads the row below and
	// reports what is actually stored.
	if err := s.store.ResolveSignal(signal.ID, req.State, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if resolutionUnparksSession(signal, req.State) {
		// Bounded to the state the dismissal was formed about, so a session
		// that has started another turn is left alone.
		s.harness.ApplyDerivedSessionState(signal.SessionID, msg.SessionIdle,
			"signal_dismissed", msg.SessionAwaitingUser)
	}

	resolved, err := s.store.GetSignal(signal.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resolved)
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
	// A todo surface asks "are any signals open against this todo?". Present
	// but empty is a 400 rather than "don't narrow": a caller that meant to
	// pass a todo id and passed nothing would otherwise get every signal in
	// the store and badge itself on somebody else's question.
	if q.Has("linked_todo_id") {
		v := strings.TrimSpace(q.Get("linked_todo_id"))
		if v == "" {
			return filter, errBadEnum("linked_todo_id", q.Get("linked_todo_id"), "a noteboard todo id")
		}
		filter.LinkedTodoID = v
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
