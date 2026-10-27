package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// writablePrincipalDirectory is a principal-store that takes the writes a
// grant of access makes: a new peer or group, the administrator flag, and
// group membership.
type writablePrincipalDirectory struct {
	mutex   sync.Mutex
	records map[string]map[string]any
	members map[string]map[string]bool
	next    int
	server  *httptest.Server
}

func newWritablePrincipalDirectory(t *testing.T) *writablePrincipalDirectory {
	t.Helper()
	directory := &writablePrincipalDirectory{records: map[string]map[string]any{}, members: map[string]map[string]bool{}, next: 100}
	directory.records[administratorTestPrincipalID] = map[string]any{"id": administratorTestPrincipalID, "kind": "human", "acts_as_caller": true,
		"restricted_to_its_grants": false, "display_name": "Root", "disabled_at": 0, "is_administrator": true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}", func(w http.ResponseWriter, r *http.Request) {
		directory.mutex.Lock()
		defer directory.mutex.Unlock()
		record, known := directory.records[r.PathValue("id")]
		if !known {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(record)
	})
	mux.HandleFunc("GET /principals/{id}/groups", func(w http.ResponseWriter, r *http.Request) {
		directory.mutex.Lock()
		defer directory.mutex.Unlock()
		groups := []map[string]any{}
		for groupID, members := range directory.members {
			if members[r.PathValue("id")] {
				groups = append(groups, directory.records[groupID])
			}
		}
		_ = json.NewEncoder(w).Encode(groups)
	})
	mux.HandleFunc("POST /principals", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kind        string `json:"kind"`
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DisplayName == "" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		directory.mutex.Lock()
		defer directory.mutex.Unlock()
		directory.next++
		id := fmt.Sprintf("principal_%06d", directory.next)
		actsAsCaller := body.Kind == "peer" || body.Kind == "human"
		directory.records[id] = map[string]any{"id": id, "kind": body.Kind, "acts_as_caller": actsAsCaller, "restricted_to_its_grants": body.Kind == "peer",
			"display_name": body.DisplayName, "disabled_at": 0, "is_administrator": false}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(directory.records[id])
	})
	mux.HandleFunc("PATCH /principals/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IsAdministrator bool `json:"is_administrator"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		directory.mutex.Lock()
		defer directory.mutex.Unlock()
		record, known := directory.records[r.PathValue("id")]
		if !known {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		record["is_administrator"] = body.IsAdministrator
		_ = json.NewEncoder(w).Encode(record)
	})
	mux.HandleFunc("PUT /principals/{group}/members/{member}", func(w http.ResponseWriter, r *http.Request) {
		directory.mutex.Lock()
		defer directory.mutex.Unlock()
		if directory.members[r.PathValue("group")] == nil {
			directory.members[r.PathValue("group")] = map[string]bool{}
		}
		directory.members[r.PathValue("group")][r.PathValue("member")] = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("DELETE /principals/{group}/members/{member}", func(w http.ResponseWriter, r *http.Request) {
		directory.mutex.Lock()
		defer directory.mutex.Unlock()
		delete(directory.members[r.PathValue("group")], r.PathValue("member"))
		w.WriteHeader(http.StatusNoContent)
	})
	directory.server = httptest.NewServer(mux)
	t.Cleanup(directory.server.Close)
	return directory
}

func (directory *writablePrincipalDirectory) isAdministrator(principalID string) bool {
	directory.mutex.Lock()
	defer directory.mutex.Unlock()
	return directory.records[principalID]["is_administrator"] == true
}

func (directory *writablePrincipalDirectory) isMember(groupID, memberID string) bool {
	directory.mutex.Lock()
	defer directory.mutex.Unlock()
	return directory.members[groupID][memberID]
}

// recordingGrantStore answers every principal's effective grants with
// nothing and records the grants made.
type recordingGrantStore struct {
	mutex   sync.Mutex
	created []grantclient.GrantInput
	server  *httptest.Server
}

func newRecordingGrantStore(t *testing.T) *recordingGrantStore {
	t.Helper()
	grants := &recordingGrantStore{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}/effective", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("POST /grants", func(w http.ResponseWriter, r *http.Request) {
		var input grantclient.GrantInput
		_ = json.NewDecoder(r.Body).Decode(&input)
		grants.mutex.Lock()
		grants.created = append(grants.created, input)
		id := fmt.Sprintf("grant_%06d", len(grants.created))
		grants.mutex.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	grants.server = httptest.NewServer(mux)
	t.Cleanup(grants.server.Close)
	return grants
}

// fakeAuthStore keeps credentials in memory.
type fakeAuthStore struct {
	mutex   sync.Mutex
	tokens  map[string]string
	deleted []string
	next    int
}

func newFakeAuthStore(t *testing.T) (*fakeAuthStore, *httptest.Server) {
	t.Helper()
	fake := &fakeAuthStore{tokens: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/credentials", func(w http.ResponseWriter, r *http.Request) {
		var input authstoreclient.CredentialInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Token == "" ||
			(input.Provider != peerCredentialProvider && input.Provider != peerConnectionCredentialProvider) {
			http.Error(w, "bad credential", http.StatusBadRequest)
			return
		}
		fake.mutex.Lock()
		fake.next++
		id := fmt.Sprintf("cred_%d", fake.next)
		fake.tokens[id] = input.Token
		fake.mutex.Unlock()
		_ = json.NewEncoder(w).Encode(authstoreclient.Credential{ID: id, Provider: input.Provider, AuthType: input.AuthType})
	})
	mux.HandleFunc("GET /api/credentials/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		fake.mutex.Lock()
		token, known := fake.tokens[r.PathValue("id")]
		fake.mutex.Unlock()
		if !known {
			http.Error(w, "unknown", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(authstoreclient.Resolved{ID: r.PathValue("id"), AuthType: "token", AccessToken: token})
	})
	mux.HandleFunc("DELETE /api/credentials/{id}", func(w http.ResponseWriter, r *http.Request) {
		fake.mutex.Lock()
		fake.deleted = append(fake.deleted, r.PathValue("id"))
		delete(fake.tokens, r.PathValue("id"))
		fake.mutex.Unlock()
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return fake, server
}

func (fake *fakeAuthStore) token(id string) string {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.tokens[id]
}

// peerTestServer is one bridge server of a pair, with stores of its own.
type peerTestServer struct {
	*gatedTestServer
	directory *writablePrincipalDirectory
	grants    *recordingGrantStore
	auth      *fakeAuthStore
}

func newPeerTestServer(t *testing.T, demoLoginEnabled bool) *peerTestServer {
	t.Helper()
	gated := newGatedTestServerConfigured(t, nil, func(cfg *config.Config) { cfg.DemoLoginEnabled = demoLoginEnabled })
	directory := newWritablePrincipalDirectory(t)
	grants := newRecordingGrantStore(t)
	auth, authServer := newFakeAuthStore(t)
	gated.server.principalClient = principalclient.New(directory.server.URL)
	gated.server.principalLookupCache = newPrincipalLookupCache(gated.server.principalClient)
	gated.server.grantClient = grantclient.New(grants.server.URL, "")
	gated.server.authClient = authstoreclient.New(authServer.URL, "test-token", "llm-bridge-server")
	return &peerTestServer{gatedTestServer: gated, directory: directory, grants: grants, auth: auth}
}

// requestAsAdministrator is an administrator's call through dash: the service
// token asserting the administrator, with a JSON body.
func (gated *gatedTestServer) requestAsAdministrator(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	request.Header.Set(serviceTokenHeader, testServiceToken)
	request.Header.Set(principalIdentityHeader, administratorTestPrincipalID)
	return serve(gated.server, request)
}

// trustTestCertificates makes every peer client trust the test servers'
// certificates, as the system's roots trust a real one.
func trustTestCertificates(t *testing.T, servers ...*httptest.Server) {
	t.Helper()
	pool := x509.NewCertPool()
	for _, server := range servers {
		pool.AddCert(server.Certificate())
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	previousHTTP, previousProxy, previousDialer := peerHTTPClient, peerProxyHTTPClient, peerSocketDialer
	peerHTTPClient = &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: refusePeerRedirect}
	peerProxyHTTPClient = &http.Client{Transport: transport, CheckRedirect: refusePeerRedirect}
	peerSocketDialer = &websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: pool}, HandshakeTimeout: 5 * time.Second}
	t.Cleanup(func() {
		peerHTTPClient, peerProxyHTTPClient, peerSocketDialer = previousHTTP, previousProxy, previousDialer
	})
}

func decodeAnswer[T any](t *testing.T, answer *httptest.ResponseRecorder, wantStatus int, what string) T {
	t.Helper()
	if answer.Code != wantStatus {
		t.Fatalf("%s = %d %s, want %d", what, answer.Code, answer.Body, wantStatus)
	}
	var value T
	if err := json.Unmarshal(answer.Body.Bytes(), &value); err != nil {
		t.Fatalf("%s: %v in %s", what, err, answer.Body)
	}
	return value
}

// connectPair makes a connection from initiating to accepting, and returns
// each side's connection id.
func connectPair(t *testing.T, accepting, initiating *peerTestServer, acceptingURL, initiatingURL string) (acceptingConnectionID, initiatingConnectionID string) {
	t.Helper()
	invite := decodeAnswer[msg.PeerConnectionInviteCreated](t, accepting.requestAsAdministrator(t, "POST", "/peer-connection-invites", map[string]any{}), http.StatusCreated, "invite")
	view := decodeAnswer[msg.PeerConnectionView](t, initiating.requestAsAdministrator(t, "POST", "/peer-connections", map[string]any{
		"base_url": acceptingURL, "passphrase": invite.Passphrase, "display_name": "accepting server", "name_for_this_server": "initiating server",
		"this_server_base_url": initiatingURL,
	}), http.StatusCreated, "connect")
	views := decodeAnswer[[]msg.PeerConnectionView](t, accepting.requestAsAdministrator(t, "GET", "/peer-connections", nil), http.StatusOK, "list on the accepting side")
	if len(views) != 1 || views[0].Connection.DisplayName != "initiating server" || views[0].Connection.Transport != view.Connection.Transport {
		t.Fatalf("the accepting side's connections = %+v", views)
	}
	again := initiating.requestAsAdministrator(t, "POST", "/peer-connections", map[string]any{
		"base_url": acceptingURL, "passphrase": invite.Passphrase, "display_name": "again", "name_for_this_server": "again",
	})
	if again.Code != http.StatusBadGateway || !strings.Contains(again.Body.String(), "already_redeemed") {
		t.Fatalf("a passphrase used twice = %d %s", again.Code, again.Body)
	}
	return views[0].Connection.ID, view.Connection.ID
}

func waitForSockets(t *testing.T, servers map[*peerTestServer]string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connected := 0
		for server, connectionID := range servers {
			if server.server.peerSockets.socketOf(connectionID) != nil {
				connected++
			}
		}
		if connected == len(servers) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the dial-out socket did not open on both sides")
}

// TestTwoServersConnectByDialingOut connects a server with no address, and
// demo login on, to one with an address, and runs access both ways over the
// socket: a grant each way, a refusal at level none, a level change, and a
// revoke that the other side hears.
func TestTwoServersConnectByDialingOut(t *testing.T) {
	accepting := newPeerTestServer(t, false)
	acceptingOverTLS := httptest.NewTLSServer(accepting.server)
	t.Cleanup(acceptingOverTLS.Close)
	trustTestCertificates(t, acceptingOverTLS)
	initiating := newPeerTestServer(t, true)

	acceptingID, initiatingID := connectPair(t, accepting, initiating, acceptingOverTLS.URL, "")
	t.Cleanup(func() { initiating.server.peerSockets.stop(initiatingID) })
	waitForSockets(t, map[*peerTestServer]string{accepting: acceptingID, initiating: initiatingID})

	if answer := initiating.requestAsAdministrator(t, "POST", "/peer-connections/"+initiatingID+"/check", map[string]any{}); answer.Code != http.StatusConflict ||
		!strings.Contains(answer.Body.String(), "peer_access_not_held") {
		t.Fatalf("a check before any grant = %d %s, want 409 peer_access_not_held", answer.Code, answer.Body)
	}

	grant := decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "administrator"}),
		http.StatusCreated, "grant administrator over the socket")
	if grant.DeliveredAt == nil || !accepting.directory.isAdministrator(grant.PeerPrincipalID) {
		t.Fatalf("the grant should be delivered and its peer an administrator: %+v", grant)
	}
	self := decodeAnswer[msg.PeerGrantSelf](t, initiating.requestAsAdministrator(t, "POST", "/peer-connections/"+initiatingID+"/check", map[string]any{}), http.StatusOK, "check")
	if self.GrantID != grant.ID || !self.IsAdministrator || self.Level != "administrator" || self.ConnectionID != acceptingID {
		t.Fatalf("check = %+v, want the administrator grant %s", self, grant.ID)
	}
	if answer := initiating.requestAsAdministrator(t, "GET", "/peer-connections/"+initiatingID+"/proxy/peer-access-levels", nil); answer.Code != http.StatusOK {
		t.Fatalf("an operator route through the proxy as an administrator peer = %d %s", answer.Code, answer.Body)
	}

	decodeAnswer[msg.PeerGrant](t, initiating.requestAsAdministrator(t, "POST", "/peer-connections/"+initiatingID+"/grant", map[string]any{"level": "none"}),
		http.StatusCreated, "grant none the other way, with demo login on")
	if answer := accepting.requestAsAdministrator(t, "GET", "/peer-connections/"+acceptingID+"/proxy/peer-access-levels", nil); answer.Code != http.StatusForbidden ||
		!strings.Contains(answer.Body.String(), "operator_route") {
		t.Fatalf("an operator route as a peer at level none = %d %s, want 403 operator_route", answer.Code, answer.Body)
	}

	level := decodeAnswer[msg.PeerAccessLevel](t, accepting.requestAsAdministrator(t, "POST", "/peer-access-levels/run_sessions/provision", map[string]any{}), http.StatusCreated, "provision")
	if level.GroupPrincipalID == "" || len(accepting.grants.created) != 3 || accepting.grants.created[0].PrincipalID != level.GroupPrincipalID {
		t.Fatalf("provisioning should make a group and its three grants: %+v %+v", level, accepting.grants.created)
	}
	if answer := accepting.requestAsAdministrator(t, "POST", "/peer-access-levels/run_sessions/provision", map[string]any{}); answer.Code != http.StatusConflict {
		t.Fatalf("provisioning twice = %d %s, want 409", answer.Code, answer.Body)
	}
	changed := decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "run_sessions"}),
		http.StatusCreated, "change the level")
	if accepting.directory.isAdministrator(changed.PeerPrincipalID) || !accepting.directory.isMember(level.GroupPrincipalID, changed.PeerPrincipalID) || changed.PeerPrincipalID != grant.PeerPrincipalID {
		t.Fatalf("a level change should take the administrator flag back and join the group, on the same peer principal: %+v", changed)
	}
	self = decodeAnswer[msg.PeerGrantSelf](t, initiating.requestAsAdministrator(t, "POST", "/peer-connections/"+initiatingID+"/check", map[string]any{}), http.StatusOK, "check after the change")
	if self.GrantID != changed.ID || self.IsAdministrator {
		t.Fatalf("check after the change = %+v, want %s and no administrator", self, changed.ID)
	}

	revoked := decodeAnswer[msg.PeerGrantRevoked](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/revoke-grant", map[string]any{"reason": "test over"}),
		http.StatusOK, "revoke")
	if revoked.Notice != "the other server was told" || accepting.directory.isMember(level.GroupPrincipalID, changed.PeerPrincipalID) {
		t.Fatalf("revoke = %+v; the group membership should be gone", revoked)
	}
	if answer := initiating.requestAsAdministrator(t, "POST", "/peer-connections/"+initiatingID+"/check", map[string]any{}); answer.Code != http.StatusConflict {
		t.Fatalf("a check after the revoke = %d %s, want 409: the other side ended its access", answer.Code, answer.Body)
	}
}

// TestTwoServersConnectDirectly connects two servers that both have an
// address, grants by https, and checks that a revoked token is refused.
func TestTwoServersConnectDirectly(t *testing.T) {
	accepting := newPeerTestServer(t, false)
	acceptingOverTLS := httptest.NewTLSServer(accepting.server)
	t.Cleanup(acceptingOverTLS.Close)
	initiating := newPeerTestServer(t, false)
	initiatingOverTLS := httptest.NewTLSServer(initiating.server)
	t.Cleanup(initiatingOverTLS.Close)
	trustTestCertificates(t, acceptingOverTLS, initiatingOverTLS)

	acceptingID, initiatingID := connectPair(t, accepting, initiating, acceptingOverTLS.URL, initiatingOverTLS.URL)
	view := decodeAnswer[msg.PeerConnectionView](t, initiating.requestAsAdministrator(t, "GET", "/peer-connections/"+initiatingID, nil), http.StatusOK, "get")
	if view.Connection.Transport != msg.PeerTransportDirect || view.SocketConnected {
		t.Fatalf("a connection between two addresses = %+v, want direct with no socket", view)
	}

	grant := decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "administrator"}),
		http.StatusCreated, "grant by https")
	self := decodeAnswer[msg.PeerGrantSelf](t, initiating.requestAsAdministrator(t, "POST", "/peer-connections/"+initiatingID+"/check", map[string]any{}), http.StatusOK, "check by https")
	if self.GrantID != grant.ID || !self.IsAdministrator {
		t.Fatalf("check = %+v", self)
	}
	held := decodeAnswer[msg.PeerConnectionView](t, initiating.requestAsAdministrator(t, "GET", "/peer-connections/"+initiatingID, nil), http.StatusOK, "get").AccessHeld
	token := initiating.auth.token(held.CredentialID)

	decodeAnswer[msg.PeerGrantRevoked](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/revoke-grant", map[string]any{"reason": "test over"}), http.StatusOK, "revoke")
	request, _ := http.NewRequest("GET", acceptingOverTLS.URL+peerSelfPath, nil)
	request.Header.Set(peerTokenHeader, token)
	answer, err := peerHTTPClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	answer.Body.Close()
	if answer.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked token = %d, want 401", answer.StatusCode)
	}

	removed := decodeAnswer[msg.PeerConnectionView](t, initiating.requestAsAdministrator(t, "POST", "/peer-connections/"+initiatingID+"/remove", map[string]any{"reason": "test over"}), http.StatusOK, "remove")
	if removed.Connection.RemovedAt == nil {
		t.Fatalf("remove = %+v", removed)
	}
}

func TestPeerConnectionRefusals(t *testing.T) {
	withDemoLogin := newPeerTestServer(t, true)
	if answer := withDemoLogin.requestAsAdministrator(t, "POST", "/peer-connection-invites", map[string]any{}); answer.Code != http.StatusConflict {
		t.Fatalf("a passphrase with demo login on = %d %s, want 409", answer.Code, answer.Body)
	}
	if answer := serve(withDemoLogin.server, httptest.NewRequest("POST", "/peer/connect", strings.NewReader(`{"passphrase":"x","server_name":"x","credential":"x"}`))); answer.Code != http.StatusConflict {
		t.Fatalf("a connect with demo login on = %d %s, want 409", answer.Code, answer.Body)
	}
	byHTTPS := httptest.NewRequest("GET", "/peer/self", nil)
	byHTTPS.Header.Set(peerTokenHeader, "anything")
	if answer := serve(withDemoLogin.server, byHTTPS); answer.Code != http.StatusConflict {
		t.Fatalf("a peer token by https with demo login on = %d %s, want 409", answer.Code, answer.Body)
	}
	if answer := withDemoLogin.requestAsAdministrator(t, "POST", "/peer-connections", map[string]any{
		"base_url": "https://other.example", "passphrase": "x", "display_name": "x", "name_for_this_server": "x", "this_server_base_url": "https://this.example",
	}); answer.Code != http.StatusConflict {
		t.Fatalf("giving this server's address with demo login on = %d %s, want 409", answer.Code, answer.Body)
	}
	if answer := withDemoLogin.requestAsAdministrator(t, "POST", "/peer-connections", map[string]any{
		"base_url": "http://other.example", "passphrase": "x", "display_name": "x", "name_for_this_server": "x",
	}); answer.Code != http.StatusBadRequest {
		t.Fatalf("an http address = %d %s, want 400", answer.Code, answer.Body)
	}

	overSocket := httptest.NewRequest("GET", "/health", nil)
	overSocket = overSocket.WithContext(context.WithValue(overSocket.Context(), peerSocketConnectionContextKey{}, "peer_connection_000001"))
	if answer := serve(withDemoLogin.server, overSocket); answer.Code != http.StatusForbidden || !strings.Contains(answer.Body.String(), "peer_socket_requires_peer_token") {
		t.Fatalf("an open route over a peer socket with no token = %d %s, want 403", answer.Code, answer.Body)
	}

	if answer := withDemoLogin.requestAsAdministrator(t, "POST", "/peer-connections/peer_connection_000009/grant", map[string]any{"level": "run_sessions"}); answer.Code != http.StatusNotFound {
		t.Fatalf("a grant on no connection = %d %s, want 404", answer.Code, answer.Body)
	}
	levels := decodeAnswer[[]msg.PeerAccessLevel](t, withDemoLogin.requestAsAdministrator(t, "GET", "/peer-access-levels", nil), http.StatusOK, "levels")
	keys := []string{}
	for _, level := range levels {
		keys = append(keys, level.Key)
	}
	if strings.Join(keys, ",") != "none,run_sessions,administrator,group" {
		t.Fatalf("levels = %v", keys)
	}
	var body bytes.Buffer
	body.WriteString(`{"level":"administrator"}`)
	request := httptest.NewRequest("POST", "/peer-connection-invites", &body)
	request.Header.Set(serviceTokenHeader, testServiceToken)
	if answer := serve(withDemoLogin.server, request); answer.Code != http.StatusForbidden {
		t.Fatalf("a change with the service token and no administrator = %d %s, want 403", answer.Code, answer.Body)
	}
}

func TestPeerBaseURLsMustBeHTTPS(t *testing.T) {
	for _, raw := range []string{"http://bridge.example.com", "https://", "https://user@bridge.example.com", "https://bridge.example.com/?q=1"} {
		if _, err := peerBaseURLOf("base_url", raw); err == nil {
			t.Errorf("%q should be refused", raw)
		}
	}
	if got, err := peerBaseURLOf("base_url", "https://dash.example.com/bridge/"); err != nil || got != "https://dash.example.com/bridge" {
		t.Errorf("https with a path = %q %v", got, err)
	}
}
