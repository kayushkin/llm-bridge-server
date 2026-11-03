package server

// Draining before a restart. deploy.sh waits for running turns and background
// agents to finish before it stops this server (GET /restart-readiness), but
// a busy server could keep starting new turns and never finish. So the deploy
// first asks the server to drain (POST /restart-drain): while it drains, a
// message that would start a turn is held instead of sent. The held message is
// kept in table held_messages and shown in its session's conversation as a
// message_held_for_restart system event, and it is sent once the drain ends:
// by this process when the deploy calls the drain off (DELETE /restart-drain)
// or the drain runs out, or by the restarted server at start
// (DeliverMessagesHeldBeforeRestart). Turns already running, and the turns a
// harness starts on its own to finish background work, are not held.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	serverids "github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// maximumRestartDrain is the longest drain a caller may ask for. A drain the
// deploy never calls off ends by itself at the time it asked for, so a deploy
// that died mid-wait cannot hold messages for longer than this.
const maximumRestartDrain = 2 * time.Hour

// restartDrainState is whether this server is draining before a restart.
type restartDrainState struct {
	mu        sync.Mutex
	draining  bool
	startedAt time.Time
	until     time.Time
	reason    string
	// expiry ends the drain at until, unless something ends it first.
	expiry *time.Timer
	// delivering lets one delivery pass run at a time.
	delivering sync.Mutex
}

// restartDrainView is the drain's state as the routes answer it.
type restartDrainView struct {
	Draining  bool       `json:"draining"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

func (s *Server) restartDrainView() restartDrainView {
	s.restartDrain.mu.Lock()
	defer s.restartDrain.mu.Unlock()
	if !s.restartDrain.draining {
		return restartDrainView{}
	}
	startedAt, until := s.restartDrain.startedAt, s.restartDrain.until
	return restartDrainView{Draining: true, StartedAt: &startedAt, Until: &until, Reason: s.restartDrain.reason}
}

type startRestartDrainRequest struct {
	MaximumSeconds int    `json:"maximum_seconds"`
	Reason         string `json:"reason"`
}

// handleStartRestartDrain serves POST /restart-drain: start holding new turns
// for at most maximum_seconds. Asking again while draining moves the end to
// the new limit.
func (s *Server) handleStartRestartDrain(w http.ResponseWriter, r *http.Request) {
	var request startRestartDrainRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	limit := time.Duration(request.MaximumSeconds) * time.Second
	if limit <= 0 || limit > maximumRestartDrain {
		writeJSONError(w, http.StatusBadRequest, "invalid_maximum_seconds",
			fmt.Sprintf("maximum_seconds must be between 1 and %d", int(maximumRestartDrain.Seconds())))
		return
	}
	if request.Reason == "" {
		writeJSONError(w, http.StatusBadRequest, "reason_required", "say why the server is draining; the log keeps it")
		return
	}
	s.startRestartDrain(limit, request.Reason)
	writeJSON(w, s.restartDrainView())
}

func (s *Server) startRestartDrain(limit time.Duration, reason string) {
	drain := &s.restartDrain
	drain.mu.Lock()
	defer drain.mu.Unlock()
	now := time.Now().UTC()
	if !drain.draining {
		drain.startedAt = now
	}
	drain.draining = true
	drain.until = now.Add(limit)
	drain.reason = reason
	if drain.expiry != nil {
		drain.expiry.Stop()
	}
	drain.expiry = time.AfterFunc(limit, func() { s.endRestartDrain("its time ran out") })
	log.Printf("[restart-drain] holding new turns until %s: %s", drain.until.Format(time.RFC3339), reason)
}

// handleEndRestartDrain serves DELETE /restart-drain: stop holding new turns
// and send the messages held so far.
func (s *Server) handleEndRestartDrain(w http.ResponseWriter, r *http.Request) {
	s.endRestartDrain("the caller ended it")
	writeJSON(w, s.restartDrainView())
}

func (s *Server) endRestartDrain(why string) {
	drain := &s.restartDrain
	drain.mu.Lock()
	wasDraining := drain.draining
	drain.draining = false
	if drain.expiry != nil {
		drain.expiry.Stop()
		drain.expiry = nil
	}
	drain.mu.Unlock()
	if wasDraining {
		log.Printf("[restart-drain] ended because %s; sending the held messages", why)
	}
	go s.deliverHeldMessages()
}

// holdMessageIfRestartDraining holds a message instead of sending it when the
// server is draining, and reports whether it did. It records the message and
// puts a message_held_for_restart event in the session's conversation. The
// check and the record happen under the drain's lock, so a message is either
// held before the drain ends, and sent by the delivery that follows, or not
// held at all.
func (s *Server) holdMessageIfRestartDraining(sessionID string, request msg.SendMessageRequest, source string) (heldMessageID string, held bool, err error) {
	drain := &s.restartDrain
	drain.mu.Lock()
	if !drain.draining {
		drain.mu.Unlock()
		return "", false, nil
	}
	heldMessageID = serverids.NewHeldMessageID()
	err = s.store.HoldMessage(store.HeldMessage{HeldMessageID: heldMessageID, SessionID: sessionID,
		Request: request, Source: source, HeldAt: time.Now()})
	drain.mu.Unlock()
	if err != nil {
		return "", false, err
	}
	log.Printf("[restart-drain] held %s for session %s (%s)", heldMessageID, sessionID, source)
	if err := s.broadcastHeldMessageEvent(sessionID, msg.SystemSubtypeMessageHeldForRestart, heldMessageID, request.Message); err != nil {
		return heldMessageID, true, fmt.Errorf("held %s but could not show it in the conversation: %w", heldMessageID, err)
	}
	return heldMessageID, true, nil
}

func (s *Server) broadcastHeldMessageEvent(sessionID, subtype, heldMessageID, message string) error {
	_, err := s.harness.BroadcastEvent(&msg.Event{
		Type:            msg.EventSystem,
		BridgeSessionID: sessionID,
		Timestamp:       time.Now().UTC(),
		System:          &msg.SystemEvent{Subtype: subtype, HeldMessageID: heldMessageID, Message: message},
	})
	return err
}

// writeHeldMessageAnswer answers a send that was held.
func writeHeldMessageAnswer(w http.ResponseWriter, heldMessageID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(msg.SendMessageResponse{Status: msg.SendStatusHeld, HeldMessageID: heldMessageID})
}

// DeliverMessagesHeldBeforeRestart sends the messages a previous process held
// while it drained before its restart. Called once from main at start.
func (s *Server) DeliverMessagesHeldBeforeRestart() {
	go s.deliverHeldMessages()
}

// deliverHeldMessages sends every held message nobody has claimed, oldest
// first, through POST /sessions/{id}/send, as if it had just arrived. It stops
// when a new drain begins; that drain's end sends the rest.
func (s *Server) deliverHeldMessages() {
	s.restartDrain.delivering.Lock()
	defer s.restartDrain.delivering.Unlock()
	held, err := s.store.UnclaimedHeldMessages()
	if err != nil {
		log.Printf("[restart-drain] ERROR: reading held messages: %v", err)
		return
	}
	for _, message := range held {
		if s.restartDrainView().Draining {
			log.Printf("[restart-drain] a new drain began; %s and later held messages wait for it to end", message.HeldMessageID)
			return
		}
		claimed, err := s.store.ClaimHeldMessage(message.HeldMessageID)
		if err != nil {
			log.Printf("[restart-drain] ERROR: %v", err)
			continue
		}
		if !claimed {
			continue
		}
		s.deliverHeldMessage(message)
	}
}

func (s *Server) deliverHeldMessage(message store.HeldMessage) {
	status, body := s.serveInternally(http.MethodPost, "/sessions/"+url.PathEscape(message.SessionID)+"/send", message.Request)
	outcome, subtype, detail := store.HeldMessageDelivered, msg.SystemSubtypeHeldMessageDelivered, ""
	if status == http.StatusAccepted {
		log.Printf("[restart-drain] held %s for session %s was held again: a new drain began while it was sent", message.HeldMessageID, message.SessionID)
		if err := s.store.RecordHeldMessageDelivery(message.HeldMessageID, store.HeldMessageHeldAgain, status, string(body)); err != nil {
			log.Printf("[restart-drain] ERROR: %v", err)
		}
		return
	}
	if status < 200 || status > 299 {
		outcome, subtype = store.HeldMessageDeliveryFailed, msg.SystemSubtypeHeldMessageDeliveryFailed
		detail = fmt.Sprintf("the send answered %d: %s", status, body)
		log.Printf("[restart-drain] ERROR: held %s for session %s was not sent: %s", message.HeldMessageID, message.SessionID, detail)
	} else {
		log.Printf("[restart-drain] sent held %s to session %s", message.HeldMessageID, message.SessionID)
	}
	if err := s.store.RecordHeldMessageDelivery(message.HeldMessageID, outcome, status, string(body)); err != nil {
		log.Printf("[restart-drain] ERROR: %v", err)
	}
	if err := s.broadcastHeldMessageEvent(message.SessionID, subtype, message.HeldMessageID, detail); err != nil {
		log.Printf("[restart-drain] ERROR: telling session %s how held %s went: %v", message.SessionID, message.HeldMessageID, err)
	}
}
