package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

const peerTestPrincipalID = "principal_000020"

func peerRecord(isAdministrator bool) string {
	record, _ := json.Marshal(map[string]any{
		"id": peerTestPrincipalID, "kind": "peer", "acts_as_caller": true, "restricted_to_its_grants": true,
		"display_name": "bridge on laptop", "disabled_at": 0, "is_administrator": isAdministrator, "groups": []any{},
	})
	return string(record)
}

// newPeerTestServer is the gated test server with demo login off, as a server
// that takes peers must have it, a peer principal in principal-store, and the
// peer's grants as given.
func newPeerTestServer(t *testing.T, peerGrants map[string][]string, peerIsAdministrator bool) *gatedTestServer {
	t.Helper()
	gated := newGatedTestServerConfigured(t, map[string]map[string][]string{
		firstTestPrincipalID: {}, secondTestPrincipalID: {}, peerTestPrincipalID: peerGrants,
	}, func(cfg *config.Config) { cfg.DemoLoginEnabled = false })
	gated.principals.setRecord(peerTestPrincipalID, peerRecord(peerIsAdministrator))
	return gated
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

// requestAsPeer presents a peer token.
func (gated *gatedTestServer) requestAsPeer(t *testing.T, token, method, path string, body any) *httptest.ResponseRecorder {
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
	request.Header.Set(peerTokenHeader, token)
	return serve(gated.server, request)
}

// inviteAndRedeem makes an invite for the peer principal and redeems it,
// returning the token.
func (gated *gatedTestServer) inviteAndRedeem(t *testing.T) msg.PeerRedeemed {
	t.Helper()
	created := gated.requestAsAdministrator(t, "POST", "/peer-invites", map[string]any{"peer_principal_id": peerTestPrincipalID})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /peer-invites = %d %s", created.Code, created.Body)
	}
	var invite msg.PeerInviteCreated
	if err := json.Unmarshal(created.Body.Bytes(), &invite); err != nil {
		t.Fatal(err)
	}
	if invite.Invite.CreatedBy != administratorTestPrincipalID || invite.Passphrase == "" {
		t.Fatalf("invite = %+v", invite)
	}
	redeemed := serve(gated.server, httptest.NewRequest("POST", "/peer/redeem", strings.NewReader(`{"passphrase":"`+invite.Passphrase+`"}`)))
	if redeemed.Code != http.StatusCreated {
		t.Fatalf("POST /peer/redeem = %d %s", redeemed.Code, redeemed.Body)
	}
	var answer msg.PeerRedeemed
	if err := json.Unmarshal(redeemed.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.PeerPrincipalID != peerTestPrincipalID || answer.Token == "" {
		t.Fatalf("redeemed = %+v", answer)
	}
	again := serve(gated.server, httptest.NewRequest("POST", "/peer/redeem", strings.NewReader(`{"passphrase":"`+invite.Passphrase+`"}`)))
	if again.Code != http.StatusConflict {
		t.Fatalf("a second redeem = %d %s, want 409: an invite makes one token", again.Code, again.Body)
	}
	return answer
}

func TestAPeerTokenActsAsItsPeerPrincipalUntilRevoked(t *testing.T) {
	gated := newPeerTestServer(t, map[string][]string{}, false)
	token := gated.inviteAndRedeem(t)

	self := gated.requestAsPeer(t, token.Token, "GET", "/peer/self", nil)
	if self.Code != http.StatusOK || !strings.Contains(self.Body.String(), peerTestPrincipalID) {
		t.Fatalf("GET /peer/self = %d %s", self.Code, self.Body)
	}
	if operator := gated.requestAsPeer(t, token.Token, "GET", "/peer-tokens", nil); operator.Code != http.StatusForbidden {
		t.Fatalf("a peer that is not an administrator on an operator route = %d %s, want 403", operator.Code, operator.Body)
	}
	if unknown := gated.requestAsPeer(t, "not-a-token", "GET", "/peer/self", nil); unknown.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown token = %d, want 401", unknown.Code)
	}
	both := httptest.NewRequest("GET", "/peer/self", nil)
	both.Header.Set(peerTokenHeader, token.Token)
	both.Header.Set(serviceTokenHeader, testServiceToken)
	if answer := serve(gated.server, both); answer.Code != http.StatusBadRequest {
		t.Fatalf("a peer token beside the service token = %d, want 400", answer.Code)
	}

	revoked := gated.requestAsAdministrator(t, "POST", "/peer-tokens/"+token.PeerTokenID+"/revoke", map[string]any{"reason": "laptop retired"})
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", revoked.Code, revoked.Body)
	}
	after := gated.requestAsPeer(t, token.Token, "GET", "/peer/self", nil)
	if after.Code != http.StatusUnauthorized || !strings.Contains(after.Body.String(), "peer_token_revoked") {
		t.Fatalf("a revoked token = %d %s, want 401 peer_token_revoked", after.Code, after.Body)
	}
}

func TestAnAdministratorPeerReachesOperatorRoutes(t *testing.T) {
	gated := newPeerTestServer(t, map[string][]string{}, true)
	token := gated.inviteAndRedeem(t)
	if answer := gated.requestAsPeer(t, token.Token, "GET", "/peer-tokens", nil); answer.Code != http.StatusOK {
		t.Fatalf("an administrator peer on an operator route = %d %s, want 200", answer.Code, answer.Body)
	}
}

// TestAPeerHoldingNoGrantMayStartNothing: a human holding no can_dispatch_on
// grant may start anywhere, and a peer may start nowhere until granted; a
// grant of "*" gives it every instance.
func TestAPeerHoldingNoGrantMayStartNothing(t *testing.T) {
	createAsPeer := func(gated *gatedTestServer, token string) *httptest.ResponseRecorder {
		return gated.requestAsPeer(t, token, "POST", "/sessions", msg.CreateSessionRequest{
			Type: msg.SessionTypeAutonomous, Purpose: msg.PurposeChat, Origin: "peer-test",
			Harness: msg.HarnessMock, InstanceID: "inst_test",
		})
	}
	ungranted := newPeerTestServer(t, map[string][]string{}, false)
	token := ungranted.inviteAndRedeem(t)
	refused := createAsPeer(ungranted, token.Token)
	if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), "restricted to its grants") {
		t.Fatalf("a peer holding no grant = %d %s, want 403 naming why", refused.Code, refused.Body)
	}

	everywhere := newPeerTestServer(t, map[string][]string{"can_dispatch_on/instance": {"*"}}, false)
	token = everywhere.inviteAndRedeem(t)
	created := createAsPeer(everywhere, token.Token)
	if created.Code != http.StatusCreated {
		t.Fatalf("a peer granted every instance = %d %s, want 201", created.Code, created.Body)
	}
	var session store.Session
	if err := json.Unmarshal(created.Body.Bytes(), &session); err != nil || session.PrincipalID != peerTestPrincipalID {
		t.Fatalf("the session belongs to the peer principal: %+v %v", session, err)
	}
}

func TestThePeerTokenIsNotForwardedToAStore(t *testing.T) {
	gated := newPeerTestServer(t, map[string][]string{}, false)
	token := gated.inviteAndRedeem(t)
	if answer := gated.requestAsPeer(t, token.Token, "GET", "/kanban/boards", nil); answer.Code != http.StatusOK {
		t.Fatalf("GET /kanban/boards as a peer = %d %s", answer.Code, answer.Body)
	}
	requests := gated.kanban.recorded()
	last := requests[len(requests)-1]
	if last.Header.Get(principalIdentityHeader) != peerTestPrincipalID {
		t.Fatalf("kanban-store should see the peer principal, saw %q", last.Header.Get(principalIdentityHeader))
	}
	if last.Header.Get(peerTokenHeader) != "" {
		t.Fatal("the peer token reached kanban-store")
	}
}

func TestPeerInvitesAreRefusedWhileDemoLoginIsOn(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	gated.principals.setRecord(peerTestPrincipalID, peerRecord(false))
	if answer := gated.requestAsAdministrator(t, "POST", "/peer-invites", map[string]any{"peer_principal_id": peerTestPrincipalID}); answer.Code != http.StatusConflict {
		t.Fatalf("an invite with demo login on = %d %s, want 409", answer.Code, answer.Body)
	}
	if answer := serve(gated.server, httptest.NewRequest("POST", "/peer/redeem", strings.NewReader(`{"passphrase":"x"}`))); answer.Code != http.StatusConflict {
		t.Fatalf("a redeem with demo login on = %d %s, want 409", answer.Code, answer.Body)
	}
}

func TestAPeerInviteIsMadeOnlyForAPeer(t *testing.T) {
	gated := newPeerTestServer(t, map[string][]string{}, false)
	answer := gated.requestAsAdministrator(t, "POST", "/peer-invites", map[string]any{"peer_principal_id": firstTestPrincipalID})
	if answer.Code != http.StatusBadRequest || !strings.Contains(answer.Body.String(), "principal_not_a_peer") {
		t.Fatalf("an invite for a human = %d %s, want 400 principal_not_a_peer", answer.Code, answer.Body)
	}
}

func TestAnExpiredInviteIsGone(t *testing.T) {
	gated := newPeerTestServer(t, map[string][]string{}, false)
	past := time.Now().Add(-2 * time.Hour)
	if err := gated.store.InsertPeerInvite(&store.PeerInvite{PassphraseHash: hashPeerSecret("old"), PeerInvite: msg.PeerInvite{PeerPrincipalID: peerTestPrincipalID,
		CreatedBy: administratorTestPrincipalID, CreatedAt: past, ExpiresAt: past.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if answer := serve(gated.server, httptest.NewRequest("POST", "/peer/redeem", strings.NewReader(`{"passphrase":"old"}`))); answer.Code != http.StatusGone {
		t.Fatalf("an expired invite = %d %s, want 410", answer.Code, answer.Body)
	}
}

func TestAPeerLinkMustBeHTTPS(t *testing.T) {
	for _, refused := range []string{"http://bridge.example.com", "bridge.example.com", "https://user@bridge.example.com", "https://bridge.example.com/?a=b"} {
		if _, err := peerBaseURLOf(refused); err == nil {
			t.Errorf("%q should be refused", refused)
		}
	}
	if got, err := peerBaseURLOf("https://bridge.example.com/api/"); err != nil || got != "https://bridge.example.com/api" {
		t.Errorf("https with a path = %q %v", got, err)
	}
}

// fakeAuthStore keeps credentials in memory for the link test.
type fakeAuthStore struct {
	mutex   sync.Mutex
	tokens  map[string]string
	deleted []string
}

func newFakeAuthStore(t *testing.T) (*fakeAuthStore, *httptest.Server) {
	t.Helper()
	fake := &fakeAuthStore{tokens: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/credentials", func(w http.ResponseWriter, r *http.Request) {
		var input authstoreclient.CredentialInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Provider != peerCredentialProvider || input.BaseURL == "" {
			http.Error(w, "bad credential", http.StatusBadRequest)
			return
		}
		fake.mutex.Lock()
		id := "cred_" + string(rune('a'+len(fake.tokens)))
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

// TestTwoServersLink runs the whole link: an administrator on the issuing
// server makes an invite, an administrator on the calling server posts its
// passphrase to POST /peer-links, which redeems it over TLS and keeps the
// token in auth-store; the check reaches the issuing server as the peer; and
// removing the link deletes the credential.
func TestTwoServersLink(t *testing.T) {
	issuing := newPeerTestServer(t, map[string][]string{}, true)
	issuingOverTLS := httptest.NewTLSServer(issuing.server)
	t.Cleanup(issuingOverTLS.Close)
	trustingTheTestCertificate := issuingOverTLS.Client()
	previousClient := peerHTTPClient
	peerHTTPClient = trustingTheTestCertificate
	t.Cleanup(func() { peerHTTPClient = previousClient })

	created := issuing.requestAsAdministrator(t, "POST", "/peer-invites", map[string]any{"peer_principal_id": peerTestPrincipalID, "expires_in_seconds": 600})
	var invite msg.PeerInviteCreated
	if err := json.Unmarshal(created.Body.Bytes(), &invite); err != nil || created.Code != http.StatusCreated {
		t.Fatalf("invite = %d %s", created.Code, created.Body)
	}

	calling := newGatedTestServer(t, nil)
	authStore, authStoreServer := newFakeAuthStore(t)
	calling.server.authClient = authstoreclient.New(authStoreServer.URL, "test-token", "llm-bridge-server")

	linked := calling.requestAsAdministrator(t, "POST", "/peer-links", map[string]any{
		"base_url": issuingOverTLS.URL, "passphrase": invite.Passphrase, "display_name": "issuing test server",
	})
	if linked.Code != http.StatusCreated {
		t.Fatalf("POST /peer-links = %d %s", linked.Code, linked.Body)
	}
	var link store.PeerLink
	if err := json.Unmarshal(linked.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}
	if link.RemotePeerPrincipalID != peerTestPrincipalID || link.CredentialID == "" || strings.Contains(linked.Body.String(), authStore.tokens[link.CredentialID]) {
		t.Fatalf("link = %s: it names the remote principal and credential and never the token", linked.Body)
	}

	checked := calling.requestAsAdministrator(t, "POST", "/peer-links/"+link.ID+"/check", map[string]any{})
	if checked.Code != http.StatusOK {
		t.Fatalf("check = %d %s", checked.Code, checked.Body)
	}
	var self msg.PeerSelf
	if err := json.Unmarshal(checked.Body.Bytes(), &self); err != nil || self.PeerPrincipalID != peerTestPrincipalID || !self.IsAdministrator {
		t.Fatalf("the issuing server should say the link acts as its administrator peer: %s", checked.Body)
	}

	removed := calling.requestAsAdministrator(t, "POST", "/peer-links/"+link.ID+"/remove", map[string]any{"reason": "test over"})
	if removed.Code != http.StatusOK || len(authStore.deleted) != 1 || authStore.deleted[0] != link.CredentialID {
		t.Fatalf("remove = %d %s; deleted %v", removed.Code, removed.Body, authStore.deleted)
	}

	plain := calling.requestAsAdministrator(t, "POST", "/peer-links", map[string]any{
		"base_url": strings.Replace(issuingOverTLS.URL, "https://", "http://", 1), "passphrase": "x", "display_name": "plain",
	})
	if plain.Code != http.StatusBadRequest {
		t.Fatalf("an http base_url = %d %s, want 400", plain.Code, plain.Body)
	}
}
