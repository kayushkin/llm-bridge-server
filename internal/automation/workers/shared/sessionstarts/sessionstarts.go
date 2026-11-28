// Package sessionstarts starts and continues sessions on llm-bridge-server
// for a job binary: POST /sessions for a new session, then POST
// /sessions/{id}/send for its turn, each with the binary's service principal
// token (bridgeauth) on the request.
//
// Until 2026-11-27 ask, autoworker and kanban-dispatcher published each turn on
// NATS (bridge.inbound) and llm-bridge-adapter made these two calls for them,
// as the internal service acting as nobody. A Turn carries what that message
// carried for the bridge — the create, the send, and the bus session id the
// adapter stamped on the create as its session_id — and Start makes the calls
// the adapter made, in its order, giving the send a client request id in the
// adapter's form when it brings none. The bus-only fields of the message
// (agent, orchestrator, channel) labeled the events the adapter published
// back on bridge.event, which nothing read, so they have no place here.
//
// Unlike a publish, every call's answer is read: a refused create or send is
// an error naming the bridge's answer, so a caller never links a card to a
// session that was never made.
package sessionstarts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/automation/clientrequestids"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/bridgesession"
)

// callTimeout bounds one call to the bridge. A create that starts its harness
// waits for the spawn.
const callTimeout = 60 * time.Second

// Turn is one turn a job binary sends into a session.
type Turn struct {
	// SessionID is the session the turn goes to. The caller mints it for a new
	// session, so it can link the id before the session exists.
	SessionID string
	// Create is the session to create first, its session_id set to SessionID
	// by Start; nil continues an existing session.
	Create *msg.CreateSessionRequest
	// Send is the message, sent after the create.
	Send msg.SendMessageRequest
}

// Start creates the turn's session when it carries a create, then sends its
// message.
func Start(bridgeURL string, turn Turn) error {
	if turn.SessionID == "" {
		return fmt.Errorf("sessionstarts: a turn names no session")
	}
	if turn.Create != nil {
		create := *turn.Create
		create.SessionID = turn.SessionID
		if err := createSession(bridgeURL, create); err != nil {
			return err
		}
	}
	return sendMessage(bridgeURL, turn.SessionID, turn.Send)
}

func createSession(bridgeURL string, create msg.CreateSessionRequest) error {
	var created msg.ManagedSession
	address := strings.TrimRight(bridgeURL, "/") + "/sessions"
	if err := postJSON(address, http.StatusCreated, create, &created); err != nil {
		return fmt.Errorf("create session %s: %w", create.SessionID, err)
	}
	if created.SessionID != create.SessionID {
		// The caller links the id it minted; a session under another id would
		// leave that link pointing nowhere.
		return fmt.Errorf("create session %s: the bridge made session %q", create.SessionID, created.SessionID)
	}
	return nil
}

func sendMessage(bridgeURL, sessionID string, send msg.SendMessageRequest) error {
	if send.ClientRequestID == "" {
		id, err := clientrequestids.New()
		if err != nil {
			return err
		}
		send.ClientRequestID = id
	}
	address, err := bridgesession.SubresourceURL(bridgeURL, sessionID, "send")
	if err != nil {
		return fmt.Errorf("send to session %s: %w", sessionID, err)
	}
	// 200 sent, or 202 held while the bridge drains before a restart, which it
	// sends once the drain ends.
	if err := postJSON(address, 0, send, nil); err != nil {
		return fmt.Errorf("send to session %s: %w", sessionID, err)
	}
	return nil
}

// postJSON posts body and insists on wantStatus, or any 2xx when it is zero,
// carrying the bridge's answer on failure.
func postJSON(address string, wantStatus int, body any, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	// No Transport of its own: the request goes out through
	// http.DefaultTransport, which main stamped with the binary's token.
	response, err := (&http.Client{Timeout: callTimeout}).Do(request)
	if err != nil {
		return fmt.Errorf("POST %s: %w", address, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read the answer to POST %s: %w", address, err)
	}
	accepted := response.StatusCode == wantStatus || (wantStatus == 0 && response.StatusCode >= 200 && response.StatusCode <= 299)
	if !accepted {
		return fmt.Errorf("POST %s answered %d: %s", address, response.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode the answer to POST %s: %w", address, err)
		}
	}
	return nil
}
