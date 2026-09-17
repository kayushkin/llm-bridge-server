package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeGrantStoreByRelation answers GET /principals/{id}/effective for one
// principal from a (relation, resource_type) → resource ids table, 404 in
// grant-store's words for any other principal.
func fakeGrantStoreByRelation(t *testing.T, principalID string, grants map[string][]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}/effective", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("id") != principalID {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found: principal ` + r.PathValue("id") + ` does not exist in principal-store"}`))
			return
		}
		key := r.URL.Query().Get("relation") + "/" + r.URL.Query().Get("resource_type")
		rows := []map[string]any{}
		for i, id := range grants[key] {
			rows = append(rows, map[string]any{"id": "grant_" + strings.Repeat("0", 5) + string(rune('1'+i)), "principal_id": principalID,
				"relation": r.URL.Query().Get("relation"), "resource_type": r.URL.Query().Get("resource_type"), "resource_id": id})
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func gatedServer(t *testing.T, grants map[string][]string) (*Server, string) {
	t.Helper()
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessMock)
	srv.principalClient = principalclient.New(fakePrincipalStore(t, "principal_000001").URL)
	srv.grantClient = grantclient.New(fakeGrantStoreByRelation(t, "principal_000001", grants).URL, "")
	return srv, instanceID
}

func createStatus(t *testing.T, srv *Server, req msg.CreateSessionRequest) (int, string) {
	t.Helper()
	resp := doJSON(t, srv, "POST", "/sessions", req)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(raw)
}

// Lenient: no can_dispatch_on grant at all means no restriction on where.
func TestGateWithNoDispatchGrantsAllowsAnyInstance(t *testing.T) {
	srv, instanceID := gatedServer(t, map[string][]string{})
	if status, body := createStatus(t, srv, principalRequest(instanceID, "principal_000001")); status != 201 {
		t.Fatalf("status = %d: %s", status, body)
	}
}

// Any can_dispatch_on grant restricts: an instance outside them is refused
// with a 403 naming what is allowed, and a machine grant covers its instances.
func TestGateRefusesAnInstanceOutsideTheDispatchGrants(t *testing.T) {
	srv, instanceID := gatedServer(t, map[string][]string{"can_dispatch_on/instance": {"inst-elsewhere"}})
	status, body := createStatus(t, srv, principalRequest(instanceID, "principal_000001"))
	if status != 403 || !strings.Contains(body, "not_granted") || !strings.Contains(body, "inst-elsewhere") {
		t.Fatalf("status = %d, want 403 not_granted naming the granted instances: %s", status, body)
	}

	byInstance, instanceID := gatedServer(t, map[string][]string{"can_dispatch_on/instance": {"inst-elsewhere"}})
	byInstance.grantClient = grantclient.New(fakeGrantStoreByRelation(t, "principal_000001", map[string][]string{"can_dispatch_on/instance": {instanceID}}).URL, "")
	if status, body := createStatus(t, byInstance, principalRequest(instanceID, "principal_000001")); status != 201 {
		t.Fatalf("granted instance: status = %d: %s", status, body)
	}

	byMachine, instanceID := gatedServer(t, map[string][]string{"can_dispatch_on/machine": {"m_test"}})
	if status, body := createStatus(t, byMachine, principalRequest(instanceID, "principal_000001")); status != 201 {
		t.Fatalf("granted machine: status = %d: %s", status, body)
	}
}

// can_run_as: no grants means any agent; grants mean only those, and a slug
// the agent-store cannot resolve is refused rather than let through.
func TestGateRefusesAnAgentOutsideTheRunAsGrants(t *testing.T) {
	srv, instanceID := gatedServer(t, map[string][]string{"can_run_as/agent": {"65"}})
	req := principalRequest(instanceID, "principal_000001")
	req.AgentID = "65"
	if status, body := createStatus(t, srv, req); status != 201 {
		t.Fatalf("granted agent by id: status = %d: %s", status, body)
	}
	req.AgentID = "12"
	if status, body := createStatus(t, srv, req); status != 403 || !strings.Contains(body, "not_granted") {
		t.Fatalf("ungranted agent: status = %d: %s", status, body)
	}
	// A slug with no agent-store to resolve it: refused, never guessed.
	req.AgentID = "northwind-builder"
	if status, body := createStatus(t, srv, req); status != 403 || !strings.Contains(body, "could not be resolved") {
		t.Fatalf("unresolvable slug: status = %d: %s", status, body)
	}
	// No can_run_as grants at all: any agent.
	open, instanceID := gatedServer(t, map[string][]string{})
	req = principalRequest(instanceID, "principal_000001")
	req.AgentID = "northwind-builder"
	if status, body := createStatus(t, open, req); status != 201 {
		t.Fatalf("no run-as grants: status = %d: %s", status, body)
	}
}

// A session with no principal is never gated, whatever grant-store would say.
func TestGateIgnoresSessionsWithoutAPrincipal(t *testing.T) {
	srv, instanceID := gatedServer(t, map[string][]string{"can_dispatch_on/instance": {"inst-elsewhere"}})
	if status, body := createStatus(t, srv, principalRequest(instanceID, "")); status != 201 {
		t.Fatalf("status = %d: %s", status, body)
	}
}

// The gate runs at spawn as well as at create, so a session created before
// its principal's grants changed is judged against the grants as they are.
func TestGateRunsAtSpawnToo(t *testing.T) {
	srv, instanceID := gatedServer(t, map[string][]string{})
	created := postCreateSession(t, srv, principalRequest(instanceID, "principal_000001"))
	srv.grantClient = grantclient.New(fakeGrantStoreByRelation(t, "principal_000001", map[string][]string{"can_dispatch_on/instance": {"inst-elsewhere"}}).URL, "")
	inst, err := srv.harnessStore.GetInstance(instanceID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = srv.startOnInstance(t.Context(), created, inst, "")
	if err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("spawn after grants changed: err = %v, want refused", err)
	}
}
