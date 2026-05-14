package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
)

// handleSidecarEvent accepts a single msg.Event from a per-session OTel
// sidecar (spawned by the PTY launch path in internal/harness/process.go)
// and routes it through bridge-server's normal event pipeline: store
// locally + push to log-store + fan out to SSE + run convenience-event
// derivation.
//
// This is the PTY-mode counterpart to the in-process OTLP receiver that
// llm-bridge-claudecode runs for -p / stream-json mode. PTY-mode sessions
// have no Go process to host an in-process receiver because the harness
// exec's straight into claude, so a sidecar process accepts OTLP from
// claude and POSTs translated events here.
//
// The bridge_id in the URL is the canonical bridge_session_id; we trust
// it over whatever the sidecar may have stamped on the event body so a
// misconfigured sidecar can't cross-pollinate sessions.
func (s *Server) handleSidecarEvent(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("bridge_id")
	if bridgeID == "" {
		http.Error(w, `{"error":"bridge_id is required"}`, http.StatusBadRequest)
		return
	}
	if s.harness == nil {
		http.Error(w, `{"error":"harness manager not configured"}`, http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
		return
	}

	var ev msg.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, `{"error":"invalid event JSON"}`, http.StatusBadRequest)
		return
	}

	// Trust the URL — the sidecar's stamped bridge_session_id is advisory.
	ev.BridgeSessionID = bridgeID

	rowID, err := s.harness.BroadcastEvent(&ev)
	if err != nil {
		log.Printf("[sidecar] broadcast event for %s: %v", bridgeID, err)
		http.Error(w, `{"error":"broadcast failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]int64{"row_id": rowID})
}
