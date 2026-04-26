package server

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"database/sql"

	harnessstore "github.com/kayushkin/harness-store"
	"github.com/gorilla/websocket"
)

var runnerUpgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	// CheckOrigin permits cross-origin WS upgrades because runners dial
	// from arbitrary networks. Auth is the bearer token in the Authorization
	// header, validated against per-machine runner_token_hash before upgrade.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// handleRunnerWS authenticates the bearer token against a Machine row,
// upgrades the request to a WebSocket, and hands the connection off to
// the harness manager's RunnerRegistry to run the Hello/Welcome handshake
// and serve until close.
func (s *Server) handleRunnerWS(w http.ResponseWriter, r *http.Request) {
	if s.harnessStore == nil {
		http.Error(w, "harness store unavailable", http.StatusServiceUnavailable)
		return
	}

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" {
		http.Error(w, "empty bearer token", http.StatusUnauthorized)
		return
	}

	machine, err := s.harnessStore.GetMachineByRunnerTokenHash(harnessstore.HashRunnerToken(token))
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	if err != nil {
		log.Printf("[runner-ws] token lookup error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	conn, err := runnerUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[runner-ws] upgrade: %v", err)
		return
	}

	if err := s.harnessStore.TouchMachineLastSeen(machine.ID); err != nil {
		log.Printf("[runner-ws] touch last_seen for %s: %v", machine.ID, err)
	}

	if err := s.harness.Runners().AcceptRunner(r.Context(), conn, machine, "llm-bridge-server"); err != nil {
		log.Printf("[runner-ws] %s connection closed: %v", machine.Name, err)
	}
}
