package server

// Each connected server knows the other by the id the other made for itself,
// and shows it by the name the other gives itself (server.name). Both travel,
// in msg.ServerIDHeader and msg.ServerNameHeader, on every request and answer
// over a connection — the socket's own dial and upgrade included — and a
// change of name is sent to every connected server at once.

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

	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// stampThisServersIdentity puts this server's id and name on an outgoing
// request or answer. An empty name says this server has none.
func (s *Server) stampThisServersIdentity(header http.Header) {
	header.Set(msg.ServerIDHeader, s.store.ThisServerID())
	header.Set(msg.ServerNameHeader, s.thisServerName())
}

// noteOtherServerIdentity records the id and name the other server put on a
// request or answer of a connection. A header that is absent comes from a
// server too old to say, and changes nothing.
func (s *Server) noteOtherServerIdentity(connectionID string, header http.Header) {
	s.noteOtherServerID(connectionID, header)
	s.noteOtherServerName(connectionID, header)
}

// noteOtherServerID records the id the other server reported. A server's id
// never changes, so a connection that reports a second id is logged as an
// error: the server behind it lost its database, or is not the one that
// connected. The newer id is recorded either way, because it is what the
// server behind the connection now answers as.
func (s *Server) noteOtherServerID(connectionID string, header http.Header) {
	values, present := header[http.CanonicalHeaderKey(msg.ServerIDHeader)]
	if !present || len(values) == 0 {
		return
	}
	serverID := strings.TrimSpace(values[0])
	if !ids.IsServerID(serverID) {
		log.Printf("[peers] ERROR: %s: the other server reported %q as its id, which is not a server id; keeping the one recorded", connectionID, values[0])
		return
	}
	replaced, written, err := s.store.RecordPeerConnectionOtherServerID(connectionID, serverID)
	if errors.Is(err, store.ErrPeerConnectionNotFound) {
		return
	}
	if err != nil {
		log.Printf("[peers] ERROR: %s: record the other server's id %s: %v", connectionID, serverID, err)
		return
	}
	switch {
	case written && replaced != "":
		log.Printf("[peers] ERROR: %s: the other server reported id %s, and it reported %s before; a server's id never changes, so it lost its database or is a different server", connectionID, serverID, replaced)
	case written:
		log.Printf("[peers] %s: the other server's id is %s", connectionID, serverID)
	}
}

// noteOtherServerName records the name the other server reported.
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

// handlePeerServerIdentity: GET /peer/server-name, a connection call. Answers
// this server's id and name; the call's own headers carry the caller's. The
// path keeps the name it had before ids, so older servers still call it.
func (s *Server) handlePeerServerIdentity(w http.ResponseWriter, r *http.Request) {
	if _, credentialError := s.connectionOfConnectionCall(r); credentialError != nil {
		credentialError.write(w)
		return
	}
	s.stampThisServersIdentity(w.Header())
	writeJSON(w, msg.PeerServerIdentity{ID: s.store.ThisServerID(), Name: s.thisServerName()})
}

// exchangeServerIdentities tells the other server this one's id and name, and
// records its answer.
func (s *Server) exchangeServerIdentities(ctx context.Context, connection store.PeerConnection) error {
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
	var answer msg.PeerServerIdentity
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&answer); err != nil {
		return fmt.Errorf("GET %s: %w", peerServerNamePath, err)
	}
	answerHeader := http.Header{http.CanonicalHeaderKey(msg.ServerNameHeader): {answer.Name}}
	if answer.ID != "" {
		answerHeader.Set(msg.ServerIDHeader, answer.ID)
	}
	s.noteOtherServerIdentity(connection.ID, answerHeader)
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
		if err := s.exchangeServerIdentities(ctx, connection); err != nil {
			log.Printf("[peers] %s: tell the other server this server's name: %v", connection.ID, err)
		}
		cancel()
	}
}
