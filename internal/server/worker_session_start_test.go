package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/access/login"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/bridgeauth"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/sessionstarts"
)

// stampRequestsAsTheWorker sends every request to bridgeURL with token, as a
// job binary's main does, and puts the process's transport back afterwards.
func stampRequestsAsTheWorker(t *testing.T, bridgeURL, token string) {
	t.Helper()
	previousTransport, previousClientTransport := http.DefaultTransport, http.DefaultClient.Transport
	t.Cleanup(func() {
		http.DefaultTransport = previousTransport
		http.DefaultClient.Transport = previousClientTransport
	})
	if err := bridgeauth.StampRequestsToBridge(bridgeURL, token); err != nil {
		t.Fatal(err)
	}
}

// writeEnvironmentRecordingLiveMockHarness puts first on PATH a mock harness
// that records its environment to the returned file and then runs the real
// mock harness, so a send into the session reaches a live process. The
// recorder that only reads its input closes its output, which ends the
// process as soon as it starts.
func writeEnvironmentRecordingLiveMockHarness(t *testing.T) string {
	t.Helper()
	mock := buildMockHarnessOnPath(t)
	directory := t.TempDir()
	recordTo := filepath.Join(t.TempDir(), "environment.txt")
	script := "#!/bin/sh\nenv > " + recordTo + ".partial && mv " + recordTo + ".partial " + recordTo + "\nexec " + mock + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(directory, msg.HarnessBinaryName(msg.HarnessMock)), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return recordTo
}

// readRecordedEnvironment waits for the harness child to record its
// environment and answers it.
func readRecordedEnvironment(t *testing.T, recordTo string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(recordTo); err == nil {
			return strings.Split(strings.TrimSpace(string(data)), "\n")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the harness child never recorded its environment")
	return nil
}

// workerTurn is the turn kanban-dispatcher starts for a card, naming the
// principal its sessions run as.
func workerTurn(sessionID, principalID string) sessionstarts.Turn {
	return sessionstarts.Turn{
		SessionID: sessionID,
		Create: &msg.CreateSessionRequest{Harness: msg.HarnessMock, InstanceID: "inst_test", PrincipalID: principalID,
			Purpose: "dispatcher", Origin: "scheduler", Type: msg.SessionTypeAutonomous, AutoStart: true},
		Send: msg.SendMessageRequest{Message: "close your card"},
	}
}

// A worker, calling over HTTP with its own service principal token through
// the code every job binary uses, starts a session as the operator only while
// it holds can_start_sessions_as on the operator. The session then runs as the
// operator, records the worker as its starter, and its agent holds the
// operator's token, which reaches the operator routes its prompt calls.
func TestAWorkerStartsSessionsAsTheOperatorOnlyWithTheGrant(t *testing.T) {
	recordTo := writeEnvironmentRecordingLiveMockHarness(t)
	server := newServicePrincipalTestServer(t)
	issued := issueServicePrincipalToken(t, server)
	bridge := httptest.NewServer(server.server)
	t.Cleanup(bridge.Close)
	stampRequestsAsTheWorker(t, bridge.URL, issued.Token)

	err := sessionstarts.Start(bridge.URL, workerTurn("tcl-refused-1", administratorTestPrincipalID))
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "principal_mismatch") {
		t.Fatalf("start as the operator with no grant: %v, want 403 principal_mismatch", err)
	}
	if _, err := server.store.GetSession("tcl-refused-1"); err == nil {
		t.Fatal("a refused create left a session behind")
	}

	server.grants.setEffective(testServicePrincipalID, login.StartSessionsAsRelation, "principal", administratorTestPrincipalID)
	if err := sessionstarts.Start(bridge.URL, workerTurn("tcl-granted-1", administratorTestPrincipalID)); err != nil {
		t.Fatalf("start as the operator holding the grant: %v", err)
	}
	t.Cleanup(func() { server.server.harness.Kill("tcl-granted-1") })
	session, err := server.store.GetSession("tcl-granted-1")
	if err != nil {
		t.Fatal(err)
	}
	if session.PrincipalID != administratorTestPrincipalID {
		t.Errorf("session principal = %q, want the operator %s", session.PrincipalID, administratorTestPrincipalID)
	}
	if starter, found, err := server.store.SessionStarterOf(session.SessionID); err != nil || !found || starter.PrincipalID != testServicePrincipalID {
		t.Errorf("starter = %+v %v %v, want the worker", starter, found, err)
	}

	var token string
	for _, entry := range readRecordedEnvironment(t, recordTo) {
		if value, found := strings.CutPrefix(entry, login.PrincipalTokenEnvironmentVariable+"="); found {
			token = value
		}
	}
	if token == "" {
		t.Fatalf("the session's harness child got no %s", login.PrincipalTokenEnvironmentVariable)
	}
	if answer := server.requestWithBearer(t, token, "GET", "/service-principal-tokens"); answer.Code != http.StatusOK {
		t.Errorf("the agent's token on an operator route = %d %s, want 200", answer.Code, answer.Body.String())
	}
}

// A worker whose own principal is an administrator is past every
// per-resource check, this one included: it starts a session as the operator
// with no grant, and no starter is recorded. This is the rule as it stands, so
// the grant is what decides only for a worker that is not an administrator.
func TestAnAdministratorWorkerStartsSessionsAsTheOperatorWithNoGrant(t *testing.T) {
	writeEnvironmentRecordingLiveMockHarness(t)
	server := newServicePrincipalTestServer(t)
	server.directory.mutex.Lock()
	server.directory.records[testServicePrincipalID]["is_administrator"] = true
	server.directory.mutex.Unlock()
	issued := issueServicePrincipalToken(t, server)
	bridge := httptest.NewServer(server.server)
	t.Cleanup(bridge.Close)
	stampRequestsAsTheWorker(t, bridge.URL, issued.Token)

	if err := sessionstarts.Start(bridge.URL, workerTurn("tcl-administrator-1", administratorTestPrincipalID)); err != nil {
		t.Fatalf("an administrator worker starting as the operator: %v", err)
	}
	t.Cleanup(func() { server.server.harness.Kill("tcl-administrator-1") })
	session, err := server.store.GetSession("tcl-administrator-1")
	if err != nil || session.PrincipalID != administratorTestPrincipalID {
		t.Fatalf("session = %+v %v, want it run as the operator", session, err)
	}
	if _, found, err := server.store.SessionStarterOf(session.SessionID); err != nil || found {
		t.Errorf("starter found = %v %v; an administrator's start records none", found, err)
	}
}
