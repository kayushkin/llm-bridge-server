package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

// attachUpgrader is dedicated to /sessions/{id}/attach so its buffer
// sizing can be tuned for raw pty bytes (terminal screens are small but
// frequent) without touching the runner-ws upgrader.
var attachUpgrader = websocket.Upgrader{
	ReadBufferSize:  16 * 1024,
	WriteBufferSize: 16 * 1024,
	// Same posture as runner-ws: this server is fronted by dash; auth
	// would belong on the proxy. Per PTY-MODE.md `[OPEN]`, the per-
	// session attach token is a child-3+ follow-up.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// pty mode wire format (single-writer, multi-reader):
//   - WebSocket text frames    → JSON control messages (see attachControl).
//   - WebSocket binary frames  → raw pty bytes, both directions.
//
// First attacher gets the writer slot; subsequent attachers are readers
// whose input frames are silently dropped. Each attacher receives a
// replay of the per-session ring buffer on connect, then live output as
// the upstream CLI emits it. Resize is a writer-only control message.

// attachControl is the schema for the JSON text frames a client sends
// to (or receives from) the attach endpoint.
type attachControl struct {
	Type   string `json:"type"`
	Code   int    `json:"code,omitempty"`
	Signal string `json:"signal,omitempty"`
	Rows   uint16 `json:"rows,omitempty"`
	Cols   uint16 `json:"cols,omitempty"`
	// Role is "writer" or "reader". Server-emitted on attach so the UI
	// can show a "read-only" affordance for non-writer observers.
	Role string `json:"role,omitempty"`
}

// handleGetAttachToken returns the current per-hub attach token for a
// pty session. Lets a client recover the token when their tab cache is
// gone (page refresh) without having to mode-switch the session to mint
// a new one. 404 if the session has no live pty hub — that's also the
// signal that mode-switch is required to spin one up.
func (s *Server) handleGetAttachToken(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	if _, err := s.store.GetSession(bridgeID); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	hub := s.harness.AttachHubFor(bridgeID)
	if hub == nil {
		http.Error(w, "session has no live pty hub", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"attach_token": hub.Token()})
}

// handleAttachSession upgrades to a WebSocket and joins the session's
// attach hub. The hub fans pty output to every connected client and
// arbitrates writer ownership; the handler is just glue between gorilla
// websocket and the hub's channels.
func (s *Server) handleAttachSession(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if sess.Mode != msg.SessionModePTY {
		http.Error(w, "session is not in pty mode", http.StatusBadRequest)
		return
	}

	hub := s.harness.AttachHubFor(bridgeID)
	if hub == nil {
		http.Error(w, "session has no live pty", http.StatusConflict)
		return
	}

	// Per-hub attach token: minted in NewAttachHub, surfaced to the
	// browser in the POST /sessions response. Constant-time compare so
	// the validation path doesn't leak token prefix length via timing.
	// Reject before upgrading so a bad token gets an HTTP 401, not a
	// WebSocket close.
	got := r.URL.Query().Get("token")
	want := hub.Token()
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		http.Error(w, "invalid attach token", http.StatusUnauthorized)
		return
	}

	conn, err := attachUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[attach %s] upgrade: %v", bridgeID, err)
		return
	}
	defer conn.Close()

	client, replay, _, err := hub.Attach()
	if err != nil {
		// Hub closed between the AttachHubFor() check and the upgrade.
		// Tell the client cleanly rather than ripping the WS down.
		if errors.Is(err, harness.ErrAttachClosed) {
			exit := attachControl{Type: "exit"}
			if data, mErr := json.Marshal(exit); mErr == nil {
				_ = conn.WriteMessage(websocket.TextMessage, data)
			}
		}
		return
	}
	defer hub.Detach(client)

	// Announce role before any other traffic. First attacher is the
	// writer (owns stdin + resize); later attachers are readers whose
	// input frames are silently dropped server-side. The UI uses this
	// to show a "read-only" affordance and to skip wiring up keystroke
	// forwarding for reader clients.
	role := "reader"
	if hub.IsWriter(client) {
		role = "writer"
	}
	if data, err := json.Marshal(attachControl{Type: "role", Role: role}); err == nil {
		if werr := conn.WriteMessage(websocket.TextMessage, data); werr != nil {
			return
		}
	}

	// Replay the ring buffer first so the client paints the current
	// screen before any new output arrives. Sent as one binary frame —
	// xterm.js handles ANSI sequences spanning frame boundaries fine,
	// but a single chunk is the simplest contract.
	if len(replay) > 0 {
		if err := conn.WriteMessage(websocket.BinaryMessage, replay); err != nil {
			return
		}
	}

	pumpAttach(r.Context(), conn, hub, client, bridgeID)
}

// pumpAttach runs the bidirectional copy between a single client
// WebSocket and its slot on the AttachHub. Returns when either side
// closes; the caller owns conn cleanup and hub.Detach.
func pumpAttach(ctx context.Context, conn *websocket.Conn, hub *harness.AttachHub, client *harness.AttachClient, bridgeID string) {
	done := make(chan struct{})
	var once sync.Once
	closeOnce := func() { once.Do(func() { close(done) }) }

	// hub → ws: pull broadcast frames off the client's output channel
	// and write them as binary WebSocket frames. Drops frames if the
	// client has been detached.
	go func() {
		defer closeOnce()
		for {
			select {
			case data, ok := <-client.Out():
				if !ok {
					return
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
					return
				}
			case <-client.Done():
				return
			}
		}
	}()

	// ws → hub. Binary frames are routed through the hub's Write (which
	// drops silently when the client isn't the writer). Text frames are
	// control JSON: resize is honored for the writer; close tears the
	// connection down.
	go func() {
		defer closeOnce()
		for {
			mt, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				if werr := hub.Write(client, payload); werr != nil {
					log.Printf("[attach %s] pty write: %v", bridgeID, werr)
					return
				}
			case websocket.TextMessage:
				var ctrl attachControl
				if err := json.Unmarshal(payload, &ctrl); err != nil {
					log.Printf("[attach %s] bad control frame: %v", bridgeID, err)
					continue
				}
				switch ctrl.Type {
				case "close":
					return
				case "resize":
					if rerr := hub.Resize(client, ctrl.Rows, ctrl.Cols); rerr != nil {
						log.Printf("[attach %s] resize %dx%d: %v", bridgeID, ctrl.Rows, ctrl.Cols, rerr)
					}
				default:
					// signal et al. land in a later child; ignore quietly
					// rather than tear the connection down.
				}
			}
		}
	}()

	// Surface a final exit control frame when the pty closes (hub
	// shutdown closes client.Done()). Best-effort: WriteMessage may
	// itself return an error if the client has already gone away.
	select {
	case <-done:
	case <-client.Done():
		exit := attachControl{Type: "exit"}
		if data, err := json.Marshal(exit); err == nil {
			_ = conn.WriteControl(websocket.TextMessage, data, time.Now().Add(2*time.Second))
			_ = conn.WriteMessage(websocket.TextMessage, data)
		}
		closeOnce()
	case <-ctx.Done():
		closeOnce()
	}
	<-done
}
