package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
)

// switchModeRequest is the JSON body of POST /sessions/{id}/mode.
type switchModeRequest struct {
	Mode msg.SessionMode `json:"mode"`
}

// handleSwitchMode flips a live session between events and pty mode.
// The harness's HarnessSessionID (Claude Code session UUID) is preserved
// across the swap, so buildStartParams.Resume kicks in and the new
// process picks up the same conversation history. In-flight turns are
// rejected — switching mid-generation would orphan a partial response.
//
// Wire shape:
//
//	POST /sessions/{id}/mode
//	{"mode":"pty"|"events"}
//
// Responses:
//   - 200 with the updated session (and attach_token when the new mode
//     is pty and start succeeded).
//   - 304 (No-op): already in the requested mode; nothing happens.
//   - 400 / 409 / etc. on validation failures.
func (s *Server) handleSwitchMode(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req switchModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate the requested mode and the harness's support for it.
	switch req.Mode {
	case msg.SessionModeEvents:
		// always supported
	case msg.SessionModePTY:
		if !harnessSupportsPTY[msg.Harness(sess.Harness)] {
			http.Error(w, `{"error":{"code":"pty_unsupported","message":"harness does not support pty mode"}}`, http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, fmt.Sprintf("invalid mode: %q", req.Mode), http.StatusBadRequest)
		return
	}

	// Idempotent: same mode = no work. 304 Not Modified is the precise
	// HTTP signal but most JSON clients trip on an empty body; return
	// 200 with the unchanged session instead so callers can always
	// JSON.parse the response.
	if sess.Mode == req.Mode {
		writeJSON(w, sess)
		return
	}

	// Refuse switching mid-generation. The harness's pause/resume hasn't
	// been hardened against partial-turn loss yet; switching during a
	// model_generating turn would drop the in-flight assistant message.
	switch msg.SessionState(sess.State) {
	case msg.SessionModelGenerating, msg.SessionToolRunning:
		http.Error(w, `{"error":{"code":"session_busy","message":"cannot switch mode while a turn is in flight"}}`, http.StatusConflict)
		return
	}

	// Kill the current process if one is running. Kill is idempotent
	// for already-dead processes — readEvents / watchPTYExit may have
	// already torn it down — so we ignore its error and trust the
	// next state observation.
	_ = s.harness.Kill(bridgeID)

	// Persist the new mode BEFORE spawning so startOnInstance (which
	// reads sess.Mode) routes through the right spawn path.
	if err := s.store.UpdateSessionMode(bridgeID, req.Mode); err != nil {
		http.Error(w, fmt.Sprintf("update mode: %v", err), http.StatusInternalServerError)
		return
	}
	sess.Mode = req.Mode

	// Resolve instance + cred and respawn. The new process inherits
	// HarnessSessionID, so --resume kicks in transparently inside
	// buildStartParams (process.go:100); the user's CC history is
	// preserved across the mode swap.
	if sess.InstanceID == "" || s.harnessStore == nil {
		http.Error(w, "session has no instance bound", http.StatusInternalServerError)
		return
	}
	inst, err := s.harnessStore.GetInstance(sess.InstanceID)
	if err != nil {
		http.Error(w, fmt.Sprintf("instance not found: %v", err), http.StatusInternalServerError)
		return
	}
	credID := resolveCredential(s.harnessStore, inst.ID)
	if _, startErr := s.startOnInstance(r.Context(), sess, inst, credID); startErr != nil {
		s.store.UpdateSessionState(bridgeID, string(msg.SessionError))
		sess.State = string(msg.SessionError)
		http.Error(w, fmt.Sprintf("spawn in %s mode: %v", req.Mode, startErr), http.StatusInternalServerError)
		return
	}
	sess.State = string(msg.SessionRunning)

	// For pty switches, surface the new hub's attach token alongside
	// the session JSON — matches the create-session response shape.
	if sess.Mode == msg.SessionModePTY {
		if hub := s.harness.AttachHubFor(bridgeID); hub != nil {
			writeJSON(w, createSessionResponse{Session: sess, AttachToken: hub.Token()})
			return
		}
	}
	writeJSON(w, sess)
}
