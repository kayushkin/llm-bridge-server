package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	agentstore "github.com/kayushkin/agent-store"
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
	srv.principalLookupCache = newPrincipalLookupCache(srv.principalClient)
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

// agentStoreWithAgents is an agent-store holding one agent per slug, and the
// id it gave each.
func agentStoreWithAgents(t *testing.T, slugs ...string) (*agentstore.Store, map[string]string) {
	t.Helper()
	agents, err := agentstore.Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agents.Close() })
	ids := map[string]string{}
	for _, slug := range slugs {
		if err := agents.UpsertAgent(&agentstore.Agent{Slug: slug, DisplayName: slug, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		agent, err := agents.GetAgentBySlug(slug)
		if err != nil {
			t.Fatal(err)
		}
		ids[slug] = strconv.FormatInt(agent.ID, 10)
	}
	return agents, ids
}

// can_run_as: no grants means any agent agent-store has; grants mean only
// those, by id or by the slug of a granted id. An agent agent-store does not
// have is refused before the grants are read, whatever they say.
func TestGateRefusesAnAgentOutsideTheRunAsGrants(t *testing.T) {
	agents, ids := agentStoreWithAgents(t, "northwind-builder", "northwind-reviewer")
	srv, instanceID := gatedServer(t, map[string][]string{"can_run_as/agent": {ids["northwind-builder"]}})
	srv.agentStore = agents
	req := principalRequest(instanceID, "principal_000001")
	req.AgentID = ids["northwind-builder"]
	if status, body := createStatus(t, srv, req); status != 201 {
		t.Fatalf("granted agent by id: status = %d: %s", status, body)
	}
	req.AgentID = "northwind-builder"
	if status, body := createStatus(t, srv, req); status != 201 {
		t.Fatalf("granted agent by slug: status = %d: %s", status, body)
	}
	req.AgentID = ids["northwind-reviewer"]
	if status, body := createStatus(t, srv, req); status != 403 || !strings.Contains(body, "not_granted") {
		t.Fatalf("ungranted agent: status = %d: %s", status, body)
	}
	req.AgentID = "nobody-has-this-agent"
	if status, body := createStatus(t, srv, req); status != 400 || !strings.Contains(body, "unknown_agent") {
		t.Fatalf("unknown agent under run-as grants: status = %d: %s", status, body)
	}
	// No can_run_as grants at all: any agent agent-store has, and still not
	// one it lacks.
	open, instanceID := gatedServer(t, map[string][]string{})
	open.agentStore = agents
	req = principalRequest(instanceID, "principal_000001")
	req.AgentID = "northwind-reviewer"
	if status, body := createStatus(t, open, req); status != 201 {
		t.Fatalf("no run-as grants: status = %d: %s", status, body)
	}
	req.AgentID = "nobody-has-this-agent"
	if status, body := createStatus(t, open, req); status != 400 || !strings.Contains(body, "unknown_agent") {
		t.Fatalf("unknown agent with no run-as grants: status = %d: %s", status, body)
	}
	req.AgentID = "999999"
	if status, body := createStatus(t, open, req); status != 400 || !strings.Contains(body, "unknown_agent") {
		t.Fatalf("unknown agent id: status = %d: %s", status, body)
	}
}

// A server with no agent-store cannot check an agent_id, so it refuses one
// rather than let it through unchecked.
func TestAnAgentIDWithNoAgentStoreIsRefused(t *testing.T) {
	srv, instanceID := gatedServer(t, map[string][]string{})
	req := principalRequest(instanceID, "principal_000001")
	req.AgentID = "northwind-builder"
	if status, body := createStatus(t, srv, req); status != 503 || !strings.Contains(body, "agent_store_not_configured") {
		t.Fatalf("status = %d, want 503 agent_store_not_configured: %s", status, body)
	}
}

// The dry run reports an agent create would refuse, as a warning.
func TestTheDryRunWarnsOfAnAgentCreateWouldRefuse(t *testing.T) {
	agents, _ := agentStoreWithAgents(t, "northwind-builder")
	srv, instanceID := gatedServer(t, map[string][]string{})
	srv.agentStore = agents
	for agentID, wantWarning := range map[string]bool{"northwind-builder": false, "nobody-has-this-agent": true} {
		resp := doJSON(t, srv, "GET", "/effective-config?instance_id="+instanceID+"&principal_id=principal_000001&agent_id="+agentID, nil)
		answer := decodeJSON[msg.EffectiveConfig](t, resp)
		warned := strings.Contains(strings.Join(answer.Warnings, "|"), "POST /sessions would refuse this session: unknown agent")
		if warned != wantWarning {
			t.Errorf("agent %s: warnings %v; want a refusal warning: %v", agentID, answer.Warnings, wantWarning)
		}
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

// The dry run reports a card create would refuse as a refusal, not only as
// lost context.
func TestTheDryRunWarnsOfACardCreateWouldRefuse(t *testing.T) {
	srv, instanceID := gatedServer(t, map[string][]string{})
	srv.kanbanClient = fakeKanbanCards(t, map[string][]string{"card-billing": {"billing"}})
	for cardID, wantWarning := range map[string]bool{"card-billing": false, "card-nobody-has": true} {
		resp := doJSON(t, srv, "GET", "/effective-config?instance_id="+instanceID+"&principal_id=principal_000001&card_id="+cardID, nil)
		answer := decodeJSON[msg.EffectiveConfig](t, resp)
		warned := strings.Contains(strings.Join(answer.Warnings, "|"), "POST /sessions would refuse this session: card_id")
		if warned != wantWarning {
			t.Errorf("card %s: warnings %v; want a refusal warning: %v", cardID, answer.Warnings, wantWarning)
		}
	}
}
