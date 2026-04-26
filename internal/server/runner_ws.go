package server

import (
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"
)

// runnerToken returns the bearer token expected from connecting runners.
// Sourced from LLMBRIDGE_RUNNER_TOKEN; empty disables the check (only
// safe on a network you fully control).
func runnerToken() string {
	return os.Getenv("LLMBRIDGE_RUNNER_TOKEN")
}

var runnerUpgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	// CheckOrigin permits cross-origin WS upgrades because runners dial
	// from arbitrary networks. Auth is handled at the Hello message via
	// bearer token, not via Origin.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// handleRunnerWS upgrades the request to a WebSocket and hands it off to
// the harness manager's RunnerRegistry, which performs the Hello/Welcome
// handshake and serves the connection until it closes.
//
// Token auth: if Authorization: Bearer <token> is present, it must match
// LLMBRIDGE_RUNNER_TOKEN. The token is also re-validated server-side
// against the Hello payload, so this header check is a fast-fail.
func (s *Server) handleRunnerWS(w http.ResponseWriter, r *http.Request) {
	expected := runnerToken()
	if expected != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	conn, err := runnerUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[runner-ws] upgrade: %v", err)
		return
	}

	if err := s.harness.Runners().AcceptRunner(r.Context(), conn, expected, "llm-bridge-server"); err != nil {
		log.Printf("[runner-ws] connection closed: %v", err)
	}
}
