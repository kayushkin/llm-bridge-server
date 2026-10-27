package server

// Each connected server shows the other by the name the other gives itself
// (server.name). The name travels in msg.ServerNameHeader on every request and
// answer over a connection — the socket's own dial and upgrade included — and
// a change is sent to every connected server at once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// stampThisServersName puts this server's name on an outgoing request or
// answer. An empty value says this server has none.
func (s *Server) stampThisServersName(header http.Header) {
	header.Set(msg.ServerNameHeader, s.thisServerName())
}

// noteOtherServerName records the name the other server put on a request or
// answer of a connection. A header that is absent comes from a server too old
// to say, and changes nothing.
func (s *Server) noteOtherServerName(connectionID string, header http.Header) {
	values, present := header[http.CanonicalHeaderKey(msg.ServerNameHeader)]
	if !present || len(values) == 0 {
		return
	}
	name := strings.TrimSpace(values[0])
	if err := checkServerNameValue(name); err != nil {
		log.Printf("[peers] %s: the other server reported a name that cannot be shown (%q): %v", connectionID, values[0], err)
		return
	}
	changed, err := s.store.RecordPeerConnectionOtherServerName(connectionID, name, time.Now())
	if errors.Is(err, store.ErrPeerConnectionNotFound) {
		return
	}
	if err != nil {
		log.Printf("[peers] ERROR: %s: record the other server's name %q: %v", connectionID, name, err)
		return
	}
	if changed {
		log.Printf("[peers] %s: the other server calls itself %q", connectionID, name)
	}
}

// firstNonEmpty returns the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// handlePeerServerName: GET /peer/server-name, a connection call. Answers this
// server's name; the call's own header carries the caller's.
func (s *Server) handlePeerServerName(w http.ResponseWriter, r *http.Request) {
	if _, credentialError := s.connectionOfConnectionCall(r); credentialError != nil {
		credentialError.write(w)
		return
	}
	s.stampThisServersName(w.Header())
	writeJSON(w, msg.PeerServerName{Name: s.thisServerName()})
}

// exchangeServerNames tells the other server this one's name and records its
// answer.
func (s *Server) exchangeServerNames(ctx context.Context, connection store.PeerConnection) error {
	response, err := s.sendOverPeerConnection(ctx, peerHTTPClient, connection, http.MethodGet, peerServerNamePath, nil, nil, 0, true)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("the other server has no %s: it is older than server names, and learns this one's from the next request", peerServerNamePath)
	}
	if response.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("GET %s: %s: %s", peerServerNamePath, response.Status, strings.TrimSpace(string(answer)))
	}
	var answer msg.PeerServerName
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&answer); err != nil {
		return fmt.Errorf("GET %s: %w", peerServerNamePath, err)
	}
	s.noteOtherServerName(connection.ID, http.Header{http.CanonicalHeaderKey(msg.ServerNameHeader): {answer.Name}})
	return nil
}

// tellConnectedServersThisServersName sends a changed name to every live
// connection. A direct one that cannot be reached now learns it from the next
// request; a dial-out one whose socket is closed learns it when it reopens.
func (s *Server) tellConnectedServersThisServersName() {
	connections, err := s.store.ListPeerConnections()
	if err != nil {
		log.Printf("[peers] ERROR: list connections to tell this server's new name: %v", err)
		return
	}
	for _, connection := range connections {
		if connection.RemovedAt != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := s.exchangeServerNames(ctx, connection); err != nil {
			log.Printf("[peers] %s: tell the other server this server's name: %v", connection.ID, err)
		}
		cancel()
	}
}
