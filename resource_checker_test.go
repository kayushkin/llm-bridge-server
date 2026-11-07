package grantstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newOwnerServer stands in for llm-bridge-server, skill-store and tool-store at
// once, answering the way each does live. Any path it does not know gets Go's
// unrouted 404, as a real ServeMux would.
func newOwnerServer(t *testing.T, agentsBody string, agentsStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/agents":
			w.WriteHeader(agentsStatus)
			w.Write([]byte(agentsBody))
		case "/instances/inst-cc-local", "/instances/inst%2Fslash", "/machines/m_localhost", "/peer-connections/peer_connection_000001":
			if r.Header.Get(LLMBridgeServiceTokenHeader) != "bridge-service-token" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"code":"service_token_required"}}`))
				return
			}
			w.Write([]byte(`{"id":"exists"}`))
		case "/skills/7", "/tools/3", "/principals/principal_000001":
			w.Write([]byte(`{"id":"exists"}`))
		case "/principals/principal_000999":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"principal not found"}`))
		case "/peer-connections/peer_connection_000999":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"code":"peer_connection_not_found"}}`))
		case "/instances/inst-gone":
			http.Error(w, "instance not found", http.StatusNotFound)
		case "/machines/m_gone":
			http.Error(w, "machine not found", http.StatusNotFound)
		case "/skills/8":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"skill not found"}`))
		case "/tools/4":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"tool not found"}`))
		case "/api/boards/board-exists":
			if r.Header.Get(KanbanStoreServiceTokenHeader) != "kanban-service-token" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"no principal and no service token"}`))
				return
			}
			w.Write([]byte(`{"id":"board-exists"}`))
		case "/api/boards/board-gone":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not found"}`))
		case "/operation-types":
			w.Write([]byte(`[{"type":"classification.run","executor":"keyword-classifier"},{"type":"llm.completion","executor":"harness-oneshot"}]`))
		case "/machines/m_boom":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCheckerAgainst(srv *httptest.Server) *HTTPResourceChecker {
	return NewHTTPResourceChecker(ResourceOwners{LLMBridgeServerURL: srv.URL + "/", LLMBridgeServiceToken: "bridge-service-token", PrincipalStoreURL: srv.URL, SkillStoreURL: srv.URL, ToolStoreURL: srv.URL, KanbanStoreURL: srv.URL, KanbanStoreServiceToken: "kanban-service-token"})
}

// TestBoardCheckSendsTheKanbanServiceToken pins the one owner that is asked
// with a credential: an enforcing kanban-store answers a bare request with 401,
// and that must read as an owner that could not answer, not a missing board.
func TestBoardCheckSendsTheKanbanServiceToken(t *testing.T) {
	srv := newOwnerServer(t, `[]`, http.StatusOK)
	ctx := context.Background()
	if err := newCheckerAgainst(srv).CheckResourceExists(ctx, ResourceTypeBoard, "board-exists"); err != nil {
		t.Fatalf("existing board with the token: %v", err)
	}
	err := newCheckerAgainst(srv).CheckResourceExists(ctx, ResourceTypeBoard, "board-gone")
	if !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("missing board: want ErrResourceNotFound, got %v", err)
	}
	withoutToken := NewHTTPResourceChecker(ResourceOwners{LLMBridgeServerURL: srv.URL, LLMBridgeServiceToken: "", PrincipalStoreURL: srv.URL, SkillStoreURL: srv.URL, ToolStoreURL: srv.URL, KanbanStoreURL: srv.URL, KanbanStoreServiceToken: ""})
	err = withoutToken.CheckResourceExists(ctx, ResourceTypeBoard, "board-exists")
	if err == nil || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("board check without the token: want an owner failure that is not ErrResourceNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("board check without the token should quote kanban-store's 401, got %v", err)
	}
}

func TestHTTPCheckerReadsTheOwnersAnswer(t *testing.T) {
	srv := newOwnerServer(t, `[{"id":12,"slug":"argraphments"},{"id":9,"slug":"bench"}]`, http.StatusOK)
	checker := newCheckerAgainst(srv)
	ctx := context.Background()

	for _, ref := range [][2]string{
		{ResourceTypeAgent, "12"},
		{ResourceTypeInstance, "inst-cc-local"},
		{ResourceTypeInstance, "inst/slash"}, // path-escaped, not split into two segments
		{ResourceTypeMachine, "m_localhost"},
		{ResourceTypeSkill, "7"},
		{ResourceTypeTool, "3"},
		{ResourceTypePrincipal, "principal_000001"},
		{ResourceTypePeerConnection, "peer_connection_000001"},
	} {
		if err := checker.CheckResourceExists(ctx, ref[0], ref[1]); err != nil {
			t.Fatalf("%s %s should exist: %v", ref[0], ref[1], err)
		}
	}
	for _, ref := range [][2]string{
		{ResourceTypeAgent, "13"},
		{ResourceTypeInstance, "inst-gone"},
		{ResourceTypeMachine, "m_gone"},
		{ResourceTypeSkill, "8"},
		{ResourceTypeTool, "4"},
		{ResourceTypePrincipal, "principal_000999"},
		{ResourceTypePeerConnection, "peer_connection_000999"},
	} {
		if err := checker.CheckResourceExists(ctx, ref[0], ref[1]); !errors.Is(err, ErrResourceNotFound) {
			t.Fatalf("%s %s: err = %v, want ErrResourceNotFound", ref[0], ref[1], err)
		}
	}
}

func TestHTTPCheckerReportsAFailingOwnerAsAFailureNotAMissingResource(t *testing.T) {
	ctx := context.Background()

	srv := newOwnerServer(t, `[]`, http.StatusOK)
	checker := newCheckerAgainst(srv)
	err := checker.CheckResourceExists(ctx, ResourceTypeMachine, "m_boom")
	if err == nil || errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("500: err = %v, want a failure carrying the status and body", err)
	}

	// A base URL pointing at a service without the route answers Go's unrouted
	// 404. That is a misconfiguration, not a missing instance.
	unrouted := NewHTTPResourceChecker(ResourceOwners{LLMBridgeServerURL: srv.URL + "/wrong-prefix", LLMBridgeServiceToken: "bridge-service-token", PrincipalStoreURL: srv.URL, SkillStoreURL: srv.URL, ToolStoreURL: srv.URL, KanbanStoreURL: srv.URL, KanbanStoreServiceToken: ""})
	err = unrouted.CheckResourceExists(ctx, ResourceTypeInstance, "inst-cc-local")
	if err == nil || errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "LLM_BRIDGE_URL") {
		t.Fatalf("unrouted 404: err = %v, want a failure naming LLM_BRIDGE_URL", err)
	}

	failingAgents := newOwnerServer(t, `database is locked`, http.StatusInternalServerError)
	err = newCheckerAgainst(failingAgents).CheckResourceExists(ctx, ResourceTypeAgent, "12")
	if err == nil || errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("agents 500: err = %v", err)
	}

	wrongShape := newOwnerServer(t, `{"agents":[{"id":12}]}`, http.StatusOK)
	err = newCheckerAgainst(wrongShape).CheckResourceExists(ctx, ResourceTypeAgent, "12")
	if err == nil || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("agents not an array: err = %v, want a failure", err)
	}

	stringIDs := newOwnerServer(t, `[{"id":"12","slug":"argraphments"}]`, http.StatusOK)
	err = newCheckerAgainst(stringIDs).CheckResourceExists(ctx, ResourceTypeAgent, "12")
	if err == nil || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("agents with string ids: err = %v, want a failure", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()
	err = NewHTTPResourceChecker(ResourceOwners{LLMBridgeServerURL: goneURL, LLMBridgeServiceToken: "", PrincipalStoreURL: goneURL, SkillStoreURL: goneURL, ToolStoreURL: goneURL, KanbanStoreURL: goneURL, KanbanStoreServiceToken: ""}).CheckResourceExists(ctx, ResourceTypeSkill, "7")
	if err == nil || errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("unreachable: err = %v", err)
	}
}

func TestHTTPCheckerTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	checker := NewHTTPResourceChecker(ResourceOwners{LLMBridgeServerURL: slow.URL, LLMBridgeServiceToken: "", PrincipalStoreURL: slow.URL, SkillStoreURL: slow.URL, ToolStoreURL: slow.URL, KanbanStoreURL: slow.URL, KanbanStoreServiceToken: ""})
	if checker.client.Timeout != ownerCheckTimeout || ownerCheckTimeout != 3*time.Second {
		t.Fatalf("client timeout = %v, want 3s", checker.client.Timeout)
	}
	checker.client.Timeout = 50 * time.Millisecond
	err := checker.CheckResourceExists(context.Background(), ResourceTypeTool, "3")
	if err == nil || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("timeout: err = %v, want a failure", err)
	}
}

func TestEveryResourceTypeHasAnOwnerCheck(t *testing.T) {
	srv := newOwnerServer(t, `[]`, http.StatusOK)
	checker := newCheckerAgainst(srv)
	for _, resourceType := range ResourceTypes {
		err := checker.CheckResourceExists(context.Background(), resourceType, "1")
		if err != nil && strings.Contains(err.Error(), "no owner check") {
			t.Fatalf("%s: %v", resourceType, err)
		}
		if _, err := lookupResourceType(resourceType); err != nil {
			t.Fatalf("%s is served but has no definition: %v", resourceType, err)
		}
	}
}

func TestOperationTypeCheckReadsTheBridgesList(t *testing.T) {
	checker := newCheckerAgainst(newOwnerServer(t, `[]`, http.StatusOK))
	if err := checker.CheckResourceExists(context.Background(), ResourceTypeOperationType, "llm.completion"); err != nil {
		t.Fatalf("known type: %v", err)
	}
	err := checker.CheckResourceExists(context.Background(), ResourceTypeOperationType, "ticket.action")
	if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "classification.run, llm.completion") {
		t.Fatalf("unknown type: %v", err)
	}
}

// llm-bridge-server answers its instance, machine and peer connection routes
// only to the service token. Without it the check is the bridge's 401,
// reported as an owner that could not answer, never as a missing resource.
func TestBridgeChecksSendTheBridgeServiceToken(t *testing.T) {
	srv := newOwnerServer(t, `[]`, http.StatusOK)
	withoutToken := NewHTTPResourceChecker(ResourceOwners{LLMBridgeServerURL: srv.URL, PrincipalStoreURL: srv.URL, SkillStoreURL: srv.URL, ToolStoreURL: srv.URL, KanbanStoreURL: srv.URL})
	for _, ref := range [][2]string{
		{ResourceTypeInstance, "inst-cc-local"},
		{ResourceTypeMachine, "m_localhost"},
		{ResourceTypePeerConnection, "peer_connection_000001"},
	} {
		err := withoutToken.CheckResourceExists(context.Background(), ref[0], ref[1])
		if err == nil || errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "401") {
			t.Errorf("%s %s without the bridge token: want the bridge's 401 as an owner failure, got %v", ref[0], ref[1], err)
		}
	}
}

func TestReferenceOwnerChecksAskReferenceStore(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /types/{type}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("type") != "prediction" {
			http.Error(w, `{"error":"no reference type"}`, http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"type":"prediction"}`))
	})
	mux.HandleFunc("POST /resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ReferenceStoreServiceTokenHeader) != "reference-token" {
			http.Error(w, "no caller", http.StatusUnauthorized)
			return
		}
		var body struct{ IDs []string }
		json.NewDecoder(r.Body).Decode(&body)
		switch body.IDs[0] {
		case "prediction_000012":
			w.Write([]byte(`{"results":{"prediction_000012":[{"type":"prediction"}]}}`))
		case "photo_000001":
			w.Write([]byte(`{"results":{"photo_000001":[]},"errors":[{"type":"photo","error":"photo-store answered 500"}]}`))
		default:
			w.Write([]byte(`{"results":{"` + body.IDs[0] + `":[]}}`))
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	checker := NewHTTPResourceChecker(ResourceOwners{ReferenceStoreURL: server.URL, ReferenceStoreServiceToken: "reference-token"})
	ctx := context.Background()
	if err := checker.CheckResourceExists(ctx, ResourceTypeReferenceType, "prediction"); err != nil {
		t.Errorf("a type that exists: %v", err)
	}
	if err := checker.CheckResourceExists(ctx, ResourceTypeReferenceType, "nonsense"); !errors.Is(err, ErrResourceNotFound) {
		t.Errorf("a type that does not: %v", err)
	}
	if err := checker.CheckResourceExists(ctx, ResourceTypeReference, "prediction_000012"); err != nil {
		t.Errorf("a record that exists: %v", err)
	}
	if err := checker.CheckResourceExists(ctx, ResourceTypeReference, "prediction_000099"); !errors.Is(err, ErrResourceNotFound) {
		t.Errorf("a record that does not: %v", err)
	}
	if err := checker.CheckResourceExists(ctx, ResourceTypeReference, "photo_000001"); err == nil || errors.Is(err, ErrResourceNotFound) {
		t.Errorf("a store that could not say is a failure, not a miss: %v", err)
	}
}
