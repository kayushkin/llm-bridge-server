package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

const (
	testServicePrincipalID = "principal_000201"
	testPersonPrincipalID  = "principal_000202"
)

// newServicePrincipalTestServer is a gated server whose principal-store knows
// a service (holds_service_principal_tokens) and a person who is no
// administrator.
func newServicePrincipalTestServer(t *testing.T) *peerTestServer {
	t.Helper()
	server := newPeerTestServer(t, false)
	server.directory.mutex.Lock()
	server.directory.records[testServicePrincipalID] = map[string]any{"id": testServicePrincipalID, "kind": "service", "acts_as_caller": true,
		"restricted_to_its_grants": true, "holds_service_principal_tokens": true, "display_name": "orchestratord", "disabled_at": 0, "is_administrator": false}
	server.directory.mutex.Unlock()
	server.directory.addPrincipal(testPersonPrincipalID, "human", false)
	return server
}

// issueServicePrincipalToken issues a token for the service as the test
// administrator and returns it.
func issueServicePrincipalToken(t *testing.T, server *peerTestServer) issuedServicePrincipalToken {
	t.Helper()
	answer := server.requestAsAdministrator(t, "POST", "/service-principal-tokens", map[string]string{"principal_id": testServicePrincipalID, "description": "orchestratord on this host"})
	issued := decodeAnswer[issuedServicePrincipalToken](t, answer, http.StatusCreated, "issue a service principal token")
	if !strings.HasPrefix(issued.Token, servicePrincipalTokenPrefix+".") || issued.Record.CreatedBy != administratorTestPrincipalID || issued.Record.PrincipalID != testServicePrincipalID {
		t.Fatalf("issued = %+v", issued)
	}
	return issued
}

// requestWithServicePrincipalToken calls the server as a service, with its
// token and nothing else.
func requestWithServicePrincipalToken(t *testing.T, server *peerTestServer, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	reader := bytes.NewReader(nil)
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer "+token)
	return serve(server.server, request)
}

func expectRefusal(t *testing.T, answer *httptest.ResponseRecorder, status int, code, what string) {
	t.Helper()
	if answer.Code != status || (code != "" && !strings.Contains(answer.Body.String(), code)) {
		t.Fatalf("%s: %d %s, want %d %s", what, answer.Code, answer.Body.String(), status, code)
	}
}

// A service's token acts as its principal, held to the route rules and to
// its grants like a person signed in as it, never as the internal service.
func TestAServicePrincipalTokenActsAsItsPrincipalHeldToItsGrants(t *testing.T) {
	server := newServicePrincipalTestServer(t)

	expectRefusal(t, server.requestAsPrincipal(t, administratorTestPrincipalID, "GET", "/service-principal-tokens", nil), http.StatusOK, "", "an administrator lists the tokens")
	bare := httptest.NewRequest("POST", "/service-principal-tokens", strings.NewReader(`{"principal_id":"`+testServicePrincipalID+`","description":"x"}`))
	bare.Header.Set(serviceTokenHeader, testServiceToken)
	expectRefusal(t, serve(server.server, bare), http.StatusForbidden, "service_principal_change_requires_a_principal", "the service token alone issues a token")
	expectRefusal(t, server.requestAsAdministrator(t, "POST", "/service-principal-tokens", map[string]string{"principal_id": testPersonPrincipalID, "description": "a person"}),
		http.StatusForbidden, "principal_does_not_hold_service_principal_tokens", "a token for a person")

	issued := issueServicePrincipalToken(t, server)
	listed := decodeAnswer[[]store.ServicePrincipalToken](t, server.requestAsAdministrator(t, "GET", "/service-principal-tokens", nil), http.StatusOK, "list tokens")
	if len(listed) != 1 || listed[0].ID != issued.Record.ID || strings.Contains(server.requestAsAdministrator(t, "GET", "/service-principal-tokens", nil).Body.String(), issued.Token) {
		t.Fatalf("the list must hold the record and never the token: %+v", listed)
	}

	person := &store.Session{SessionID: "br_person", PrincipalID: testPersonPrincipalID, InstanceID: "inst_test", Harness: msg.HarnessMock, State: string(msg.SessionIdle)}
	if err := server.store.CreateSession(person); err != nil {
		t.Fatal(err)
	}
	sessions := decodeAnswer[[]store.Session](t, requestWithServicePrincipalToken(t, server, issued.Token, "GET", "/sessions", nil), http.StatusOK, "list sessions holding no observe grant")
	if len(sessions) != 0 {
		t.Fatalf("a service with no grant sees only its own sessions, and has none: %+v", sessions)
	}
	server.grants.setEffective(testServicePrincipalID, observeSessionsRelation, "instance", "*")
	sessions = decodeAnswer[[]store.Session](t, requestWithServicePrincipalToken(t, server, issued.Token, "GET", "/sessions", nil), http.StatusOK, "list sessions observing every instance")
	if len(sessions) != 1 || sessions[0].SessionID != "br_person" {
		t.Fatalf("observing every instance lists every session: %+v", sessions)
	}
	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "POST", "/sessions/br_person/rename", map[string]string{"display_name": "mine now"}),
		http.StatusNotFound, "", "observing is not driving")
	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "GET", "/service-principal-tokens", nil), http.StatusForbidden, "operator_route", "a service on an operator route")

	public := httptest.NewRequest("GET", "/sessions", nil)
	public.Header.Set("Authorization", "Bearer "+issued.Token)
	public.Header.Set("X-Forwarded-For", "203.0.113.9")
	expectRefusal(t, serve(server.server, public), http.StatusForbidden, "service_principal_token_not_public", "a token through the public proxy")
	expectRefusal(t, requestWithServicePrincipalToken(t, server, servicePrincipalTokenPrefix+".made-up", "GET", "/sessions", nil), http.StatusUnauthorized, "invalid_service_principal_token", "a made-up token")

	revoked := server.requestAsAdministrator(t, "POST", "/service-principal-tokens/"+issued.Record.ID+"/revoke", map[string]string{"reason": "test over"})
	if record := decodeAnswer[store.ServicePrincipalToken](t, revoked, http.StatusOK, "revoke"); record.RevokedBy != administratorTestPrincipalID || record.RevokeReason != "test over" {
		t.Fatalf("revoked record = %+v", record)
	}
	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "GET", "/sessions", nil), http.StatusUnauthorized, "service_principal_token_revoked", "a revoked token")
	expectRefusal(t, server.requestAsAdministrator(t, "POST", "/service-principal-tokens/"+issued.Record.ID+"/revoke", map[string]string{"reason": "again"}),
		http.StatusConflict, "service_principal_token_already_revoked", "revoking twice")
}

// A service starts a session as a person only while it holds
// can_start_sessions_as on them, and then drives that session and the ones
// it starts under it, and no other session of theirs.
func TestAServiceStartsAndDrivesSessionsAsAPrincipalItHoldsAGrantOn(t *testing.T) {
	server := newServicePrincipalTestServer(t)
	issued := issueServicePrincipalToken(t, server)
	request := msg.CreateSessionRequest{Type: msg.SessionTypeAutonomous, Purpose: "orchestrator", Origin: "agent-test",
		Harness: msg.HarnessMock, InstanceID: "inst_test", PrincipalID: testPersonPrincipalID}

	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "POST", "/sessions", request), http.StatusForbidden, "principal_mismatch", "starting as a person with no grant")

	server.grants.setEffective(testServicePrincipalID, startSessionsAsRelation, "principal", testPersonPrincipalID)
	home := decodeAnswer[store.Session](t, requestWithServicePrincipalToken(t, server, issued.Token, "POST", "/sessions", request), http.StatusCreated, "start a session as the person")
	if home.PrincipalID != testPersonPrincipalID {
		t.Fatalf("the session runs as the person, not the service: %+v", home)
	}
	if starter, found, err := server.store.SessionStarterOf(home.SessionID); err != nil || !found || starter.PrincipalID != testServicePrincipalID || starter.Backfilled {
		t.Fatalf("starter = %+v %v %v", starter, found, err)
	}
	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "POST", "/sessions/"+home.SessionID+"/rename", map[string]string{"display_name": "Orchestrator"}),
		http.StatusOK, "", "the starter renames its session")
	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "GET", "/sessions/"+home.SessionID, nil), http.StatusOK, "", "the starter reads its session")

	underHome := request
	underHome.ManagerSessionID = home.SessionID
	sweep := decodeAnswer[store.Session](t, requestWithServicePrincipalToken(t, server, issued.Token, "POST", "/sessions", underHome), http.StatusCreated, "start a session under it")
	if sweep.ManagerSessionID != home.SessionID {
		t.Fatalf("sweep = %+v", sweep)
	}

	person := &store.Session{SessionID: "br_person_chat", PrincipalID: testPersonPrincipalID, InstanceID: "inst_test", Harness: msg.HarnessMock, State: string(msg.SessionIdle)}
	if err := server.store.CreateSession(person); err != nil {
		t.Fatal(err)
	}
	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "POST", "/sessions/br_person_chat/rename", map[string]string{"display_name": "x"}),
		http.StatusNotFound, "", "a session of the person's the service did not start")

	server.grants.setEffective(testServicePrincipalID, startSessionsAsRelation, "principal")
	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "POST", "/sessions/"+home.SessionID+"/rename", map[string]string{"display_name": "x"}),
		http.StatusNotFound, "", "the starter once the grant is revoked")
}

// A session created before starters were recorded gets one from an
// administrator, and only for a service that holds the grant.
func TestAnAdministratorRecordsTheStarterOfAnOlderSession(t *testing.T) {
	server := newServicePrincipalTestServer(t)
	issued := issueServicePrincipalToken(t, server)
	older := &store.Session{SessionID: "br_older_home", PrincipalID: testPersonPrincipalID, InstanceID: "inst_test", Harness: msg.HarnessMock, State: string(msg.SessionIdle)}
	if err := server.store.CreateSession(older); err != nil {
		t.Fatal(err)
	}
	body := map[string]string{"principal_id": testServicePrincipalID}
	expectRefusal(t, server.requestAsAdministrator(t, "PUT", "/sessions/br_older_home/starter", body), http.StatusConflict, "not_granted", "a starter with no grant")
	server.grants.setEffective(testServicePrincipalID, startSessionsAsRelation, "principal", testPersonPrincipalID)
	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "PUT", "/sessions/br_older_home/starter", body), http.StatusForbidden, "operator_route", "the service records itself")
	recorded := decodeAnswer[store.SessionStarter](t, server.requestAsAdministrator(t, "PUT", "/sessions/br_older_home/starter", body), http.StatusOK, "record the starter")
	if !recorded.Backfilled || recorded.RecordedBy != administratorTestPrincipalID || recorded.PrincipalID != testServicePrincipalID {
		t.Fatalf("recorded = %+v", recorded)
	}
	expectRefusal(t, server.requestAsAdministrator(t, "PUT", "/sessions/br_older_home/starter", body), http.StatusConflict, "session_starter_already_recorded", "recording twice")
	expectRefusal(t, requestWithServicePrincipalToken(t, server, issued.Token, "POST", "/sessions/br_older_home/rename", map[string]string{"display_name": "Orchestrator"}),
		http.StatusOK, "", "the recorded starter drives it")
}

// A service lists and calls through only the connections its
// can_call_through grants name, and the connected server sees it named as
// itself, so it reads there what that server's mapping and grants allow.
// A person holding no such grant is not restricted on the proxy, as before
// the relation existed, and lists no connection, as before.
func TestAServiceCallsThroughOnlyTheConnectionsItIsGranted(t *testing.T) {
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	grant := decodeAnswer[msg.PeerGrant](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/grant", map[string]any{"level": "none"}),
		http.StatusCreated, "grant none, so the test sets the peer's grants itself")
	peerID := grant.PeerPrincipalID
	initiating.directory.mutex.Lock()
	initiating.directory.records[testServicePrincipalID] = map[string]any{"id": testServicePrincipalID, "kind": "service", "acts_as_caller": true,
		"restricted_to_its_grants": true, "holds_service_principal_tokens": true, "display_name": "orchestratord", "disabled_at": 0, "is_administrator": false}
	initiating.directory.mutex.Unlock()
	initiating.directory.addPrincipal(testPersonPrincipalID, "human", false)
	issued := issueServicePrincipalToken(t, initiating)
	asService := func(method, path string) *httptest.ResponseRecorder {
		return requestWithServicePrincipalToken(t, initiating, issued.Token, method, path, nil)
	}
	proxy := "/peer-connections/" + initiatingID + "/proxy"

	if listed := decodeAnswer[[]msg.PeerConnectionView](t, asService("GET", "/peer-connections"), http.StatusOK, "list with no grant"); len(listed) != 0 {
		t.Fatalf("a service with no can_call_through grant lists %+v, want none", listed)
	}
	expectRefusal(t, asService("GET", "/peer-connections/"+initiatingID), http.StatusNotFound, "peer_connection_not_found", "one connection with no grant")
	expectRefusal(t, asService("GET", proxy+"/sessions"), http.StatusForbidden, "peer_connection_not_granted", "the proxy with no grant")

	initiating.grants.setEffective(testServicePrincipalID, callThroughRelation, "peer_connection", initiatingID)
	if listed := decodeAnswer[[]msg.PeerConnectionView](t, asService("GET", "/peer-connections"), http.StatusOK, "list with the grant"); len(listed) != 1 || listed[0].Connection.ID != initiatingID {
		t.Fatalf("a service granted one connection lists %+v", listed)
	}
	expectRefusal(t, asService("GET", "/peer-connections/"+initiatingID), http.StatusOK, "", "one connection with the grant")
	answer := asService("GET", proxy+"/sessions")
	expectRefusal(t, answer, http.StatusForbidden, "peer_user_not_mapped", "the proxy before the other server maps the service")
	if !strings.Contains(answer.Body.String(), testServicePrincipalID) {
		t.Fatalf("the other server should name the service it does not know: %s", answer.Body)
	}

	const localReader = "principal_000701"
	accepting.directory.addPrincipal(localReader, "human", false)
	decodeAnswer[msg.PeerUserMapping](t, accepting.requestAsAdministrator(t, "POST", "/peer-user-mappings", map[string]any{
		"peer_principal_id": peerID, "remote_principal_id": testServicePrincipalID, "local_principal_id": localReader,
	}), http.StatusCreated, "map the service on the other server")
	accepting.grants.setEffective(peerID, observeSessionsRelation, "instance", "*")
	accepting.grants.setEffective(localReader, observeSessionsRelation, "instance", "*")
	if err := accepting.server.store.CreateSession(&store.Session{SessionID: "br_far", Harness: msg.HarnessMock, InstanceID: "inst_test", State: string(msg.SessionIdle), PrincipalID: "principal_000801"}); err != nil {
		t.Fatal(err)
	}
	far := decodeAnswer[[]store.Session](t, asService("GET", proxy+"/sessions"), http.StatusOK, "the other server's sessions, mapped and observing")
	if len(far) != 1 || far[0].SessionID != "br_far" {
		t.Fatalf("the service reads %+v there, want br_far", far)
	}

	person := initiating.requestAsPrincipal(t, testPersonPrincipalID, "GET", proxy+"/sessions", nil)
	if strings.Contains(person.Body.String(), "peer_connection_not_granted") || !strings.Contains(person.Body.String(), "peer_user_not_mapped") {
		t.Fatalf("a person with no grant reaches the other server as before: %d %s", person.Code, person.Body)
	}
	if listed := decodeAnswer[[]msg.PeerConnectionView](t, initiating.requestAsPrincipal(t, testPersonPrincipalID, "GET", "/peer-connections", nil), http.StatusOK, "a person lists"); len(listed) != 0 {
		t.Fatalf("a person with no grant lists %+v, want none", listed)
	}
}

// GET /self tells a service which principal its token acts as, so the id is
// never configured beside the token.
func TestSelfNamesThePrincipalATokenActsAs(t *testing.T) {
	server := newServicePrincipalTestServer(t)
	issued := issueServicePrincipalToken(t, server)
	self := decodeAnswer[msg.CallerSelf](t, requestWithServicePrincipalToken(t, server, issued.Token, "GET", "/self", nil), http.StatusOK, "self as the service")
	if self.PrincipalID != testServicePrincipalID || self.IsAdministrator || self.InternalService {
		t.Fatalf("self = %+v", self)
	}
	administrator := decodeAnswer[msg.CallerSelf](t, server.requestAsAdministrator(t, "GET", "/self", nil), http.StatusOK, "self as an administrator")
	if administrator.PrincipalID != administratorTestPrincipalID || !administrator.IsAdministrator {
		t.Fatalf("administrator self = %+v", administrator)
	}
	bare := httptest.NewRequest("GET", "/self", nil)
	bare.Header.Set(serviceTokenHeader, testServiceToken)
	if internal := decodeAnswer[msg.CallerSelf](t, serve(server.server, bare), http.StatusOK, "self as the service token"); !internal.InternalService || internal.PrincipalID != "" {
		t.Fatalf("internal self = %+v", internal)
	}
	expectRefusal(t, serve(server.server, httptest.NewRequest("GET", "/self", nil)), http.StatusUnauthorized, "", "self with no credential")
}
