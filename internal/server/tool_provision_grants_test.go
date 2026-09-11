package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeGrantStore answers GET /principals/{id}/effective with the given tool
// ids for the one principal it knows, 404 in grant-store's words for any other.
func fakeGrantStore(t *testing.T, principalID string, toolIDs []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}/effective", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != principalID {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found: principal ` + r.PathValue("id") + ` does not exist in principal-store"}`))
			return
		}
		if r.URL.Query().Get("relation") != "can_use" || r.URL.Query().Get("resource_type") != "tool" {
			http.Error(w, "the spawn must ask for can_use tools only", 400)
			return
		}
		var rows []map[string]any
		for i, id := range toolIDs {
			rows = append(rows, map[string]any{"id": "grant_00000" + string(rune('1'+i)), "principal_id": principalID, "relation": "can_use", "resource_type": "tool", "resource_id": id})
		}
		if rows == nil {
			rows = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeToolStoreWithOptIns serves an instance's opt-in list and a /provision
// that records what it was asked for and answers one server per requested id.
func fakeToolStoreWithOptIns(t *testing.T, optIns map[string][]map[string]any) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var provisionBodies []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /instances/{id}/tools", func(w http.ResponseWriter, r *http.Request) {
		rows := optIns[r.PathValue("id")]
		if rows == nil {
			rows = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	})
	mux.HandleFunc("POST /provision", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		provisionBodies = append(provisionBodies, body)
		servers := map[string]any{}
		if ids, ok := body["tool_ids"].([]any); ok {
			for _, id := range ids {
				servers[fmt.Sprintf("tool-%v", id)] = map[string]any{"command": "x"}
			}
		}
		if _, byInstance := body["instance_id"]; byInstance {
			servers["instance-default"] = map[string]any{"command": "y"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"mcpServers": servers})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &provisionBodies
}

func newServerWithGrants(toolStoreURL, grantStoreURL string) *Server {
	return &Server{cfg: &config.Config{ToolStoreURL: toolStoreURL}, grantClient: grantclient.New(grantStoreURL)}
}

var mcpTool13 = map[string]any{"id": 13, "name": "playwright", "kind": "mcp"}
var mcpTool14 = map[string]any{"id": 14, "name": "chrome-devtools", "kind": "mcp"}
var mcpTool12 = map[string]any{"id": 12, "name": "brave-search", "kind": "mcp"}
var cliTool7 = map[string]any{"id": 7, "name": "scheduler", "kind": "cli"}

// The grant path: only what is both granted to the principal and opted in
// on the instance is offered, and it is asked for by id.
func TestInjectMCPConfigOffersOnlyGrantedToolsWiredOnTheInstance(t *testing.T) {
	grants := fakeGrantStore(t, "principal_000001", []string{"13", "7", "99"})
	tools, asked := fakeToolStoreWithOptIns(t, map[string][]map[string]any{"inst-1": {mcpTool13, mcpTool14, cliTool7}})
	s := newServerWithGrants(tools.URL, grants.URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", PrincipalID: "principal_000001"}

	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*asked) != 1 {
		t.Fatalf("tool-store /provision was asked %d times, want 1: %v", len(*asked), *asked)
	}
	ids, _ := (*asked)[0]["tool_ids"].([]any)
	if _, byInstance := (*asked)[0]["instance_id"]; byInstance || len(ids) != 1 || ids[0] != float64(13) {
		t.Fatalf("provision body = %v, want tool_ids [13] only (13 is granted and wired; 14 is wired but not granted; 7 is granted but a CLI tool; 99 is granted but not wired)", (*asked)[0])
	}
	path := mcpConfigPath(t, sess)
	if path == "" {
		t.Fatal("granted tools did not reach the session")
	}
	t.Cleanup(func() { os.Remove(path) })
}

// Lenient, by the operator's choice: a principal with no can_use tool grant at
// all is offered the instance's opt-ins exactly as a session with no principal.
func TestInjectMCPConfigPrincipalWithNoGrantsGetsTheInstanceOptIns(t *testing.T) {
	grants := fakeGrantStore(t, "principal_000002", nil)
	tools, asked := fakeToolStoreWithOptIns(t, map[string][]map[string]any{"inst-1": {mcpTool12}})
	s := newServerWithGrants(tools.URL, grants.URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", PrincipalID: "principal_000002"}

	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0]["instance_id"] != "inst-1" {
		t.Fatalf("provision body = %v, want the instance opt-in request", *asked)
	}
	path := mcpConfigPath(t, sess)
	if path == "" {
		t.Fatal("the instance opt-ins did not reach the session")
	}
	t.Cleanup(func() { os.Remove(path) })
}

// Granted tools that the instance has not wired are not offered, and nothing
// is provisioned rather than something the principal was not granted.
func TestInjectMCPConfigNothingBothGrantedAndWiredStartsWithNoServers(t *testing.T) {
	grants := fakeGrantStore(t, "principal_000001", []string{"13"})
	tools, asked := fakeToolStoreWithOptIns(t, map[string][]map[string]any{"inst-1": {mcpTool12}})
	s := newServerWithGrants(tools.URL, grants.URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", PrincipalID: "principal_000001", HarnessConfig: json.RawMessage(`{"settings":"{}"}`)}
	before := string(sess.HarnessConfig)

	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*asked) != 0 {
		t.Fatalf("tool-store /provision was asked with %v; nothing should have been provisioned", *asked)
	}
	if string(sess.HarnessConfig) != before {
		t.Fatalf("HarnessConfig was mutated: before=%s after=%s", before, sess.HarnessConfig)
	}
}

// A session that named a principal must not be offered everything because the
// grants could not be read: an unreachable grant-store, or one that does not
// know the principal, aborts the spawn with the reason.
func TestInjectMCPConfigAbortsWhenTheGrantsCannotBeRead(t *testing.T) {
	tools, asked := fakeToolStoreWithOptIns(t, map[string][]map[string]any{"inst-1": {mcpTool13}})

	unknown := fakeGrantStore(t, "principal_000001", []string{"13"})
	s := newServerWithGrants(tools.URL, unknown.URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", PrincipalID: "principal_000404"}
	err := s.injectMCPConfig(sess)
	if err == nil || !strings.Contains(err.Error(), "principal unknown") {
		t.Fatalf("unknown principal: err = %v, want the spawn aborted naming the principal", err)
	}

	down := fakeGrantStore(t, "principal_000001", []string{"13"})
	downURL := down.URL
	down.Close()
	s = newServerWithGrants(tools.URL, downURL)
	sess = &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", PrincipalID: "principal_000001"}
	err = s.injectMCPConfig(sess)
	if err == nil || !strings.Contains(err.Error(), "grant-store unreachable") {
		t.Fatalf("grant-store down: err = %v, want the spawn aborted", err)
	}
	if len(*asked) != 0 {
		t.Fatalf("tool-store was asked despite the grants being unknown: %v", *asked)
	}
}

// A session with no principal never touches grant-store, even when one is
// configured — today's behaviour, pinned.
func TestInjectMCPConfigWithoutAPrincipalNeverAsksGrantStore(t *testing.T) {
	grantsAsked := false
	grants := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grantsAsked = true
		http.Error(w, "should not be called", 500)
	}))
	t.Cleanup(grants.Close)
	tools, _ := fakeToolStoreWithOptIns(t, map[string][]map[string]any{"inst-1": {mcpTool12}})
	s := newServerWithGrants(tools.URL, grants.URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1"}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if grantsAsked {
		t.Fatal("grant-store was asked about a session with no principal")
	}
	if path := mcpConfigPath(t, sess); path != "" {
		t.Cleanup(func() { os.Remove(path) })
	}
}
