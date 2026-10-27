package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func nameServer(t *testing.T, server *peerTestServer, name string) {
	t.Helper()
	if answer := server.requestAsAdministrator(t, "PUT", "/settings/server.name", map[string]string{"value": name}); answer.Code != http.StatusOK {
		t.Fatalf("PUT server.name %q = %d %s", name, answer.Code, answer.Body)
	}
}

// waitForOtherServerName waits until a connection shows the name the other
// server reported.
func waitForOtherServerName(t *testing.T, server *peerTestServer, connectionID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var view msg.PeerConnectionView
	for time.Now().Before(deadline) {
		view = decodeAnswer[msg.PeerConnectionView](t, server.requestAsAdministrator(t, "GET", "/peer-connections/"+connectionID, nil), http.StatusOK, "get")
		if view.OtherServerName == want && view.OtherServerNameReportedAt != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("connection %s shows the other server as %q, want %q", connectionID, view.OtherServerName, want)
}

// connectByNamesAlone connects with no typed labels: each side is known by the
// name it reports.
func connectByNamesAlone(t *testing.T, accepting, initiating *peerTestServer, acceptingURL, initiatingURL string) (acceptingID, initiatingID string) {
	t.Helper()
	invite := decodeAnswer[msg.PeerConnectionInviteCreated](t, accepting.requestAsAdministrator(t, "POST", "/peer-connection-invites", map[string]any{}), http.StatusCreated, "invite")
	body := map[string]any{"base_url": acceptingURL, "passphrase": invite.Passphrase}
	if initiatingURL != "" {
		body["this_server_base_url"] = initiatingURL
	}
	view := decodeAnswer[msg.PeerConnectionView](t, initiating.requestAsAdministrator(t, "POST", "/peer-connections", body), http.StatusCreated, "connect")
	views := decodeAnswer[[]msg.PeerConnectionView](t, accepting.requestAsAdministrator(t, "GET", "/peer-connections", nil), http.StatusOK, "list")
	if len(views) != 1 {
		t.Fatalf("the accepting side's connections = %+v", views)
	}
	return views[0].Connection.ID, view.Connection.ID
}

func TestServerNamesTravelOverADialOutConnection(t *testing.T) {
	accepting := newPeerTestServer(t, false)
	acceptingOverTLS := httptest.NewTLSServer(accepting.server)
	t.Cleanup(acceptingOverTLS.Close)
	trustTestCertificates(t, acceptingOverTLS)
	initiating := newPeerTestServer(t, true)

	invite := decodeAnswer[msg.PeerConnectionInviteCreated](t, accepting.requestAsAdministrator(t, "POST", "/peer-connection-invites", map[string]any{}), http.StatusCreated, "invite")
	if answer := initiating.requestAsAdministrator(t, "POST", "/peer-connections", map[string]any{"base_url": acceptingOverTLS.URL, "passphrase": invite.Passphrase}); answer.Code != http.StatusBadRequest ||
		!strings.Contains(answer.Body.String(), "this_server_has_no_name") {
		t.Fatalf("connecting with no server.name and no label = %d %s, want 400 this_server_has_no_name", answer.Code, answer.Body)
	}

	nameServer(t, accepting, "main")
	nameServer(t, initiating, "laptop")
	if answer := initiating.requestAsAdministrator(t, "PUT", "/settings/server.name", map[string]string{"value": "two\nlines"}); answer.Code != http.StatusBadRequest {
		t.Fatalf("a name with a line break = %d %s, want 400", answer.Code, answer.Body)
	}
	described := decodeAnswer[msg.ServerDescription](t, initiating.requestAsAdministrator(t, "GET", "/server", nil), http.StatusOK, "GET /server")
	if described.Name != "laptop" || described.KeepAwake.Enabled || described.KeepAwake.Held {
		t.Fatalf("GET /server = %+v, want laptop with keep-awake off", described)
	}

	acceptingID, initiatingID := connectByNamesAlone(t, accepting, initiating, acceptingOverTLS.URL, "")
	t.Cleanup(func() { initiating.server.peerSockets.stop(initiatingID) })
	waitForSockets(t, map[*peerTestServer]string{accepting: acceptingID, initiating: initiatingID})
	waitForOtherServerName(t, initiating, initiatingID, "main")
	waitForOtherServerName(t, accepting, acceptingID, "laptop")

	nameServer(t, accepting, "home server")
	waitForOtherServerName(t, initiating, initiatingID, "home server")
	nameServer(t, initiating, "work laptop")
	waitForOtherServerName(t, accepting, acceptingID, "work laptop")
	nameServer(t, initiating, "")
	waitForOtherServerName(t, accepting, acceptingID, "")
}

func TestServerNamesTravelOverADirectConnection(t *testing.T) {
	accepting := newPeerTestServer(t, false)
	acceptingOverTLS := httptest.NewTLSServer(accepting.server)
	t.Cleanup(acceptingOverTLS.Close)
	initiating := newPeerTestServer(t, false)
	initiatingOverTLS := httptest.NewTLSServer(initiating.server)
	t.Cleanup(initiatingOverTLS.Close)
	trustTestCertificates(t, acceptingOverTLS, initiatingOverTLS)
	nameServer(t, accepting, "main")
	nameServer(t, initiating, "desk")

	acceptingID, initiatingID := connectByNamesAlone(t, accepting, initiating, acceptingOverTLS.URL, initiatingOverTLS.URL)
	waitForOtherServerName(t, initiating, initiatingID, "main")
	waitForOtherServerName(t, accepting, acceptingID, "desk")

	nameServer(t, accepting, "renamed main")
	waitForOtherServerName(t, initiating, initiatingID, "renamed main")

	// An answer to a peer request carries the name too: a grant delivered by
	// https brings the receiving server's name back with it.
	nameServer(t, initiating, "renamed desk")
	waitForOtherServerName(t, accepting, acceptingID, "renamed desk")
	decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "none"}),
		http.StatusCreated, "grant by https")
	waitForOtherServerName(t, accepting, acceptingID, "renamed desk")
}

func TestGetServerReportsTheKeepAwakeSettings(t *testing.T) {
	server := newPeerTestServer(t, false)
	if answer := server.requestAsAdministrator(t, "PUT", "/settings/keep_awake.grace_minutes", map[string]string{"value": "-5"}); answer.Code != http.StatusBadRequest {
		t.Fatalf("a negative grace = %d %s, want 400", answer.Code, answer.Body)
	}
	if answer := server.requestAsAdministrator(t, "PUT", "/settings/keep_awake.enabled", map[string]string{"value": "true"}); answer.Code != http.StatusOK {
		t.Fatalf("switch keep-awake on = %d %s", answer.Code, answer.Body)
	}
	described := decodeAnswer[msg.ServerDescription](t, server.requestAsAdministrator(t, "GET", "/server", nil), http.StatusOK, "GET /server")
	if !described.KeepAwake.Enabled || described.KeepAwake.Held {
		t.Fatalf("GET /server = %+v, want enabled and, with no controller started, not held", described)
	}
	if answer := serve(server.server, httptest.NewRequest("GET", "/server", nil)); answer.Code != http.StatusUnauthorized {
		t.Fatalf("GET /server with no credential = %d %s, want 401", answer.Code, answer.Body)
	}
}
