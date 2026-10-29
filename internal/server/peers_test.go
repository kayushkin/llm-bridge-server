package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
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

// addPrincipal records a principal of a kind that acts as a caller.
func (directory *writablePrincipalDirectory) addPrincipal(principalID, kind string, isAdministrator bool) {
	directory.mutex.Lock()
	defer directory.mutex.Unlock()
	directory.records[principalID] = map[string]any{"id": principalID, "kind": kind, "acts_as_caller": true, "restricted_to_its_grants": kind == "peer",
		"display_name": principalID, "disabled_at": 0, "is_administrator": isAdministrator}
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

// recordingGrantStore records the grants made, and answers a principal's
// effective grants with what the test set (setEffective), nothing otherwise.
type recordingGrantStore struct {
	mutex     sync.Mutex
	created   []grantclient.GrantInput
	effective map[string][]string
	server    *httptest.Server
}

func effectiveGrantKey(principalID, relation, resourceType string) string {
	return principalID + "|" + relation + "|" + resourceType
}

// setEffective makes a principal's effective grants of one relation on one
// resource type name exactly these resources.
func (grants *recordingGrantStore) setEffective(principalID, relation, resourceType string, resourceIDs ...string) {
	grants.mutex.Lock()
	defer grants.mutex.Unlock()
	grants.effective[effectiveGrantKey(principalID, relation, resourceType)] = resourceIDs
}

func newRecordingGrantStore(t *testing.T) *recordingGrantStore {
	t.Helper()
	grants := &recordingGrantStore{effective: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}/effective", func(w http.ResponseWriter, r *http.Request) {
		grants.mutex.Lock()
		resourceIDs := grants.effective[effectiveGrantKey(r.PathValue("id"), r.URL.Query().Get("relation"), r.URL.Query().Get("resource_type"))]
		grants.mutex.Unlock()
		answer := []map[string]string{}
		for index, resourceID := range resourceIDs {
			answer = append(answer, map[string]string{"id": fmt.Sprintf("grant_%06d", index+1), "principal_id": r.PathValue("id"),
				"resource_type": r.URL.Query().Get("resource_type"), "resource_id": resourceID})
		}
		_ = json.NewEncoder(w).Encode(answer)
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

func newPeerTestServer(t *testing.T, demoLoginEnabled bool, configure ...func(*config.Config)) *peerTestServer {
	t.Helper()
	// A data directory of its own, so two connected test servers never share
	// the files they keep beside their database (attachments, updates).
	dataDirectory := t.TempDir()
	gated := newGatedTestServerConfigured(t, nil, func(cfg *config.Config) {
		cfg.DemoLoginEnabled = demoLoginEnabled
		cfg.DBPath = filepath.Join(dataDirectory, "bridge.db")
		for _, change := range configure {
			change(cfg)
		}
	})
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

// requestAsPrincipal is a call through dash for a principal of this server
// who need not be an administrator.
func (gated *gatedTestServer) requestAsPrincipal(t *testing.T, principalID, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set(serviceTokenHeader, testServiceToken)
	request.Header.Set(principalIdentityHeader, principalID)
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
	if strings.Join(keys, ",") != "none,run_sessions,messages,observe,administrator,group" {
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

// connectedPairByDialingOut connects a server with no address to one with
// one and waits for the socket.
func connectedPairByDialingOut(t *testing.T, configureAccepting ...func(*config.Config)) (accepting, initiating *peerTestServer, acceptingID, initiatingID string) {
	t.Helper()
	accepting = newPeerTestServer(t, false, configureAccepting...)
	acceptingOverTLS := httptest.NewTLSServer(accepting.server)
	t.Cleanup(acceptingOverTLS.Close)
	trustTestCertificates(t, acceptingOverTLS)
	initiating = newPeerTestServer(t, true)
	acceptingID, initiatingID = connectPair(t, accepting, initiating, acceptingOverTLS.URL, "")
	t.Cleanup(func() { initiating.server.peerSockets.stop(initiatingID) })
	waitForSockets(t, map[*peerTestServer]string{accepting: acceptingID, initiating: initiatingID})
	return accepting, initiating, acceptingID, initiatingID
}

// TestPeerUsersActAsMappedPrincipalsWithinBothGrants is phase 2: a user of
// the calling server is refused until mapped, then acts as the mapped
// principal, allowed only what both it and the peer may do, reaching only the
// sessions it started through the peer unless both may observe more, and
// refused the routes this server cannot narrow.
func TestPeerUsersActAsMappedPrincipalsWithinBothGrants(t *testing.T) {
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	grant := decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "none"}),
		http.StatusCreated, "grant none, so the test sets the peer's grants itself")
	peerID := grant.PeerPrincipalID
	const remoteUser = "principal_000501"
	const localUser = "principal_000601"
	initiating.directory.addPrincipal(remoteUser, "human", false)
	accepting.directory.addPrincipal(localUser, "human", false)
	accepting.grants.setEffective(peerID, "can_dispatch_on", "instance", "inst_test", "inst_second")
	accepting.grants.setEffective(localUser, "can_dispatch_on", "instance", "inst_second")
	proxy := "/peer-connections/" + initiatingID + "/proxy"
	asUser := func(method, path string) *httptest.ResponseRecorder {
		return initiating.requestAsPrincipal(t, remoteUser, method, proxy+path, nil)
	}

	if answer := asUser("GET", "/instances"); answer.Code != http.StatusForbidden || !strings.Contains(answer.Body.String(), "peer_user_not_mapped") ||
		!strings.Contains(answer.Body.String(), remoteUser) {
		t.Fatalf("an unmapped user = %d %s, want 403 peer_user_not_mapped naming the user", answer.Code, answer.Body)
	}
	if answer := accepting.requestAsAdministrator(t, "POST", "/peer-user-mappings", map[string]any{
		"peer_principal_id": "principal_999999", "remote_principal_id": remoteUser, "local_principal_id": localUser,
	}); answer.Code != http.StatusBadRequest {
		t.Fatalf("a mapping for a peer principal no connection names = %d %s, want 400", answer.Code, answer.Body)
	}
	mapping := decodeAnswer[msg.PeerUserMapping](t, accepting.requestAsAdministrator(t, "POST", "/peer-user-mappings", map[string]any{
		"peer_principal_id": peerID, "remote_principal_id": remoteUser, "local_principal_id": localUser,
	}), http.StatusCreated, "map")
	if mapping.CreatedBy != administratorTestPrincipalID {
		t.Fatalf("mapping = %+v, want it to record the administrator", mapping)
	}

	instances := decodeAnswer[[]msg.Instance](t, asUser("GET", "/instances"), http.StatusOK, "instances as the mapped user")
	if len(instances) != 1 || instances[0].ID != "inst_second" {
		t.Fatalf("the mapped user sees %+v, want only inst_second, the one both the peer and the user may start on", instances)
	}
	accepting.grants.setEffective(peerID, "can_dispatch_on", "instance", "inst_test")
	if instances := decodeAnswer[[]msg.Instance](t, asUser("GET", "/instances"), http.StatusOK, "instances"); len(instances) != 0 {
		t.Fatalf("with no instance in common the mapped user sees %+v, want none", instances)
	}

	for _, seed := range []string{"br_local_own", "br_through_peer"} {
		if seed == "br_through_peer" {
			if err := accepting.server.store.RecordSessionPeerOrigin(store.SessionPeerOrigin{SessionID: seed, PeerPrincipalID: peerID, RemotePrincipalID: remoteUser}); err != nil {
				t.Fatal(err)
			}
		}
		if err := accepting.server.store.CreateSession(&store.Session{SessionID: seed, Harness: "mock", InstanceID: "inst_test", State: "idle", PrincipalID: localUser}); err != nil {
			t.Fatal(err)
		}
	}
	if answer := asUser("GET", "/sessions/br_local_own"); answer.Code != http.StatusNotFound {
		t.Fatalf("the local principal's own session, not started through the peer = %d %s, want 404", answer.Code, answer.Body)
	}
	if answer := asUser("GET", "/sessions/br_through_peer"); answer.Code != http.StatusOK {
		t.Fatalf("a session started through the peer for the user = %d %s, want 200", answer.Code, answer.Body)
	}
	listed := decodeAnswer[[]msg.ManagedSession](t, asUser("GET", "/sessions"), http.StatusOK, "list")
	if len(listed) != 1 || listed[0].SessionID != "br_through_peer" {
		t.Fatalf("the mapped user's list = %+v, want only br_through_peer", listed)
	}

	accepting.grants.setEffective(localUser, observeSessionsRelation, "instance", "*")
	if answer := asUser("GET", "/sessions/br_local_own"); answer.Code != http.StatusNotFound {
		t.Fatalf("observing with only the user's grant = %d, want 404: the peer may not observe", answer.Code)
	}
	accepting.grants.setEffective(peerID, observeSessionsRelation, "instance", "*")
	if answer := asUser("GET", "/sessions/br_local_own"); answer.Code != http.StatusOK {
		t.Fatalf("observing with both grants = %d %s, want 200", answer.Code, answer.Body)
	}
	if listed := decodeAnswer[[]msg.ManagedSession](t, asUser("GET", "/sessions"), http.StatusOK, "list while observing"); len(listed) != 2 {
		t.Fatalf("observing every instance, the list = %d sessions, want 2", len(listed))
	}
	if answer := asUser("POST", "/sessions/br_local_own/stop"); answer.Code != http.StatusNotFound {
		t.Fatalf("stopping an observed session = %d %s, want 404: observing changes nothing", answer.Code, answer.Body)
	}
	if answer := asUser("GET", "/kanban/boards"); answer.Code != http.StatusForbidden || !strings.Contains(answer.Body.String(), "route_not_for_peer_users") {
		t.Fatalf("a store proxy as a mapped user = %d %s, want 403 route_not_for_peer_users", answer.Code, answer.Body)
	}

	removed := decodeAnswer[msg.PeerUserMapping](t, accepting.requestAsAdministrator(t, "POST", "/peer-user-mappings/"+mapping.ID+"/remove", map[string]any{"reason": "test over"}),
		http.StatusOK, "remove the mapping")
	if removed.RemovedAt == nil || removed.RemovedBy != administratorTestPrincipalID {
		t.Fatalf("removed mapping = %+v", removed)
	}
	if answer := asUser("GET", "/instances"); answer.Code != http.StatusForbidden {
		t.Fatalf("after the mapping is removed = %d, want 403", answer.Code)
	}

	sent := httptest.NewRequest("GET", "/instances", nil)
	sent.Header.Set(serviceTokenHeader, testServiceToken)
	sent.Header.Set(peerUserHeader, remoteUser)
	if answer := serve(accepting.server, sent); answer.Code != http.StatusBadRequest || !strings.Contains(answer.Body.String(), "peer_user_without_peer_token") {
		t.Fatalf("a user header with no peer token = %d %s, want 400", answer.Code, answer.Body)
	}
}

// TestRemovingAConnectionTellsTheOtherServer removes a dial-out connection
// on the side that dials and checks the other side marks it removed.
func TestRemovingAConnectionTellsTheOtherServer(t *testing.T) {
	previous := peerSocketCloseAfterRemovalNotice
	peerSocketCloseAfterRemovalNotice = 50 * time.Millisecond
	t.Cleanup(func() { peerSocketCloseAfterRemovalNotice = previous })
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "administrator"}), http.StatusCreated, "grant")

	removed := decodeAnswer[msg.PeerConnectionView](t, initiating.requestAsAdministrator(t, "POST", "/peer-connections/"+initiatingID+"/remove", map[string]any{"reason": "laptop retired"}),
		http.StatusOK, "remove")
	if removed.Connection.RemovalNotice != "the other server was told" {
		t.Fatalf("removal notice = %q", removed.Connection.RemovalNotice)
	}
	other := decodeAnswer[msg.PeerConnectionView](t, accepting.requestAsAdministrator(t, "GET", "/peer-connections/"+acceptingID, nil), http.StatusOK, "the other side")
	if other.Connection.RemovedAt == nil || !other.Connection.RemovedByOtherServer || other.Connection.RemoveReason != "laptop retired" || other.AccessGiven != nil {
		t.Fatalf("the other side's connection = %+v, want it removed by the other server with its access revoked", other)
	}
}

// TestSessionEventsStreamThroughTheSocket reads a stream that never ends
// through the proxy over a dial-out socket, and gets its first frame while it
// is still open.
func TestSessionEventsStreamThroughTheSocket(t *testing.T) {
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "administrator"}), http.StatusCreated, "grant")
	initiatingOverHTTP := httptest.NewServer(initiating.server)
	t.Cleanup(initiatingOverHTTP.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", initiatingOverHTTP.URL+"/peer-connections/"+initiatingID+"/proxy/session-events", nil)
	request.Header.Set(serviceTokenHeader, testServiceToken)
	request.Header.Set(principalIdentityHeader, administratorTestPrincipalID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("session-events through the socket = %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "event: hello" {
		t.Fatalf("first line through the socket = %q %v, want event: hello while the stream is still open", line, err)
	}
}

// TestTheProxyNeverPassesBackAPageForThisOrigin has the other server answer
// HTML with a cookie and a redirect: the browser must get none of it as a page
// of this server's origin.
func TestTheProxyNeverPassesBackAPageForThisOrigin(t *testing.T) {
	accepting := newPeerTestServer(t, false)
	acceptingOverTLS := httptest.NewTLSServer(accepting.server)
	t.Cleanup(acceptingOverTLS.Close)
	initiating := newPeerTestServer(t, false)
	initiatingOverTLS := httptest.NewTLSServer(initiating.server)
	t.Cleanup(initiatingOverTLS.Close)
	trustTestCertificates(t, acceptingOverTLS, initiatingOverTLS)
	acceptingID, initiatingID := connectPair(t, accepting, initiating, acceptingOverTLS.URL, initiatingOverTLS.URL)
	decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "administrator"}),
		http.StatusCreated, "grant")

	const pattern = "GET /an-answer-for-the-proxy-test"
	answers := map[string]string{"html": "text/html; charset=utf-8", "json": "application/json; charset=utf-8", "stream": "text/event-stream"}
	accepting.server.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", answers[r.URL.Query().Get("kind")])
		w.Header().Set("Set-Cookie", "stolen=1")
		w.Header().Set("Refresh", "0; url=https://elsewhere.example")
		w.Header().Set("Content-Security-Policy", "default-src *")
		_, _ = w.Write([]byte("<script>alert(1)</script>"))
	})
	routeAccessRules[pattern] = openRoute("a test answer")
	t.Cleanup(func() { delete(routeAccessRules, pattern) })

	for kind, want := range map[string]string{"html": "application/octet-stream", "json": answers["json"], "stream": answers["stream"]} {
		answer := initiating.requestAsAdministrator(t, "GET", "/peer-connections/"+initiatingID+"/proxy/an-answer-for-the-proxy-test?kind="+kind, nil)
		if answer.Code != http.StatusOK {
			t.Fatalf("%s through the proxy = %d %s", kind, answer.Code, answer.Body)
		}
		header := answer.Header()
		if got := header.Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type = %q, want %q", kind, got, want)
		}
		if header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Content-Security-Policy") != "sandbox" {
			t.Errorf("%s: nosniff = %q, policy = %q, want nosniff and sandbox", kind, header.Get("X-Content-Type-Options"), header.Get("Content-Security-Policy"))
		}
		if header.Get("Set-Cookie") != "" || header.Get("Refresh") != "" {
			t.Errorf("%s: the other server's Set-Cookie %q or Refresh %q reached the browser", kind, header.Get("Set-Cookie"), header.Get("Refresh"))
		}
	}
}

func TestPassedBackContentTypeKeepsOnlyWhatTheBridgeAnswers(t *testing.T) {
	for peerType, want := range map[string]string{
		"application/json": "application/json", "application/json; charset=utf-8": "application/json; charset=utf-8",
		"text/event-stream": "text/event-stream", "text/html": "application/octet-stream", "image/svg+xml": "application/octet-stream",
		"image/png": "image/png", "image/jpeg": "image/jpeg", "image/gif": "image/gif", "image/webp": "image/webp", "image/bmp": "application/octet-stream",
		"": "application/octet-stream", "not a type": "application/octet-stream", "Application/JSON": "Application/JSON",
	} {
		if got := passedBackContentType(peerType); got != want {
			t.Errorf("passedBackContentType(%q) = %q, want %q", peerType, got, want)
		}
	}
}

// TestAPeerSocketServesOnlySoManyRequestsAtOnce holds the most requests one
// socket runs at once open on the accepting side, and checks the next is
// answered 503 at once instead of being started.
func TestAPeerSocketServesOnlySoManyRequestsAtOnce(t *testing.T) {
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "administrator"}), http.StatusCreated, "grant")
	const pattern = "GET /a-request-that-waits"
	release := make(chan struct{})
	started := make(chan struct{}, peerSocketMostRequestsServedAtOnce)
	accepting.server.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)
	})
	routeAccessRules[pattern] = openRoute("a test answer")
	t.Cleanup(func() { delete(routeAccessRules, pattern) })
	initiatingOverHTTP := httptest.NewServer(initiating.server)
	t.Cleanup(initiatingOverHTTP.Close)
	get := func() (*http.Response, error) {
		request, _ := http.NewRequest("GET", initiatingOverHTTP.URL+"/peer-connections/"+initiatingID+"/proxy/a-request-that-waits", nil)
		request.Header.Set(serviceTokenHeader, testServiceToken)
		request.Header.Set(principalIdentityHeader, administratorTestPrincipalID)
		return http.DefaultClient.Do(request)
	}
	results := make(chan int, peerSocketMostRequestsServedAtOnce)
	for range peerSocketMostRequestsServedAtOnce {
		go func() {
			response, err := get()
			if err != nil {
				results <- -1
				return
			}
			response.Body.Close()
			results <- response.StatusCode
		}()
	}
	for range peerSocketMostRequestsServedAtOnce {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("the held requests did not all start")
		}
	}
	response, err := get()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "peer_socket_busy") {
		t.Fatalf("one request past the most = %d %s, want 503 peer_socket_busy", response.StatusCode, body)
	}
	close(release)
	for range peerSocketMostRequestsServedAtOnce {
		if code := <-results; code != http.StatusNoContent {
			t.Fatalf("a held request ended %d, want 204", code)
		}
	}
}

// TestALargeAnswerCrossesThePeerSocketInChunks writes one answer larger than
// the chunk a frame carries and reads it back whole.
func TestALargeAnswerCrossesThePeerSocketInChunks(t *testing.T) {
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "administrator"}), http.StatusCreated, "grant")
	const pattern = "GET /a-large-answer"
	large := bytes.Repeat([]byte("0123456789abcdef"), 3*peerSocketLargestAnswerChunk/16+5)
	accepting.server.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(large)
	})
	routeAccessRules[pattern] = openRoute("a test answer")
	t.Cleanup(func() { delete(routeAccessRules, pattern) })
	answer := initiating.requestAsAdministrator(t, "GET", "/peer-connections/"+initiatingID+"/proxy/a-large-answer", nil)
	if answer.Code != http.StatusOK || !bytes.Equal(answer.Body.Bytes(), large) {
		t.Fatalf("large answer = %d, %d bytes, want 200 and %d bytes", answer.Code, answer.Body.Len(), len(large))
	}
}

// TestAFrameOverTheReadLimitEndsTheSocket sends one message larger than the
// accepting side reads and checks that side closes the socket.
func TestAFrameOverTheReadLimitEndsTheSocket(t *testing.T) {
	previous := peerSocketReadLimit
	peerSocketReadLimit = 4096
	t.Cleanup(func() { peerSocketReadLimit = previous })
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	acceptingSocket := accepting.server.peerSockets.socketOf(acceptingID)
	initiatingSocket := initiating.server.peerSockets.socketOf(initiatingID)
	if acceptingSocket == nil || initiatingSocket == nil {
		t.Fatal("a side has no open socket")
	}
	if err := initiatingSocket.send(peerSocketFrame{Type: peerSocketFrameCancel, Stream: 1, Error: strings.Repeat("x", 8192)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-acceptingSocket.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the accepting side kept a socket that sent a frame over its read limit")
	}
}

// TestAPeerStartedSessionsAgentIsNoAdministratorThroughAPeerThatIsNot maps a
// connected server's user to a local administrator through a peer that is not
// one, and checks that session's agent, using its posting token, does not get
// an administrator's unfiltered instance list.
func TestAPeerStartedSessionsAgentIsNoAdministratorThroughAPeerThatIsNot(t *testing.T) {
	accepting := newPeerTestServer(t, false)
	const localAdministrator = "principal_000701"
	const peerID = "principal_000702"
	accepting.directory.addPrincipal(localAdministrator, "human", true)
	accepting.directory.addPrincipal(peerID, "peer", false)
	accepting.grants.setEffective(localAdministrator, "can_dispatch_on", "instance", "inst_second")
	accepting.grants.setEffective(peerID, "can_dispatch_on", "instance", "inst_second")
	st := accepting.server.store
	for _, session := range []*store.Session{
		{SessionID: "br_local_admins_own", Harness: "claude_code", State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive, PrincipalID: localAdministrator},
		{SessionID: "br_started_through_peer", Harness: "claude_code", State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive, PrincipalID: localAdministrator},
	} {
		if err := st.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordSessionPeerOrigin(store.SessionPeerOrigin{SessionID: "br_started_through_peer", PeerPrincipalID: peerID, RemotePrincipalID: "principal_000703"}); err != nil {
		t.Fatal(err)
	}
	instancesAsAgentOf := func(sessionID string) []msg.Instance {
		recorder := servedWithPostingToken(t, accepting.server, currentPostingTokenFor(t, accepting.server, sessionID), http.MethodGet, "/instances", nil)
		return decodeAnswer[[]msg.Instance](t, recorder, http.StatusOK, "instances as the agent of "+sessionID)
	}
	all := instancesAsAgentOf("br_local_admins_own")
	if len(all) < 2 {
		t.Fatalf("the local administrator's own agent sees %d instances, want every one (at least 2)", len(all))
	}
	throughPeer := instancesAsAgentOf("br_started_through_peer")
	if len(throughPeer) != 1 || throughPeer[0].ID != "inst_second" {
		t.Fatalf("the agent of a session started through a non-administrator peer sees %+v, want only inst_second", throughPeer)
	}
}
