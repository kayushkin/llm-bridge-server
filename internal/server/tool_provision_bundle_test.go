package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/bundleclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeBundleStore answers GET /bundles/{id} and POST /resolve for one bundle
// it knows, in bundle-store's own words for everything else: 404 for a
// missing id, 400 "invalid id" for a name, and /resolve refusing an id it
// lacks with 404 rather than composing a smaller bundle.
func fakeBundleStore(t *testing.T, bundleID string, toolIDs []int64, skillIDs []int64) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var resolveBodies []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bundles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if strings.Trim(id, "0123456789") != "" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid id"}`))
			return
		}
		if id != bundleID {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"bundle not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 6, "name": "docker", "enabled": true})
	})
	mux.HandleFunc("POST /resolve", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		resolveBodies = append(resolveBodies, body)
		ids, _ := body["bundle_ids"].([]any)
		if len(ids) != 1 || ids[0] != float64(6) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"bundle not applicable: bundle id does not exist"}`))
			return
		}
		var tools, skills []map[string]any
		for _, id := range toolIDs {
			tools = append(tools, map[string]any{"id": id, "name": "tool"})
		}
		for _, id := range skillIDs {
			skills = append(skills, map[string]any{"id": id, "name": "skill"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bundles": []map[string]any{{"id": 1, "name": "base"}, {"id": 6, "name": "docker"}},
			"skills":  skills, "tools": tools,
			"denied_tools": []map[string]any{}, "denied_read_paths": []string{},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &resolveBodies
}

func newServerWithBundles(toolStoreURL, bundleStoreURL, grantStoreURL string) *Server {
	s := &Server{cfg: &config.Config{ToolStoreURL: toolStoreURL}, bundleClient: bundleclient.New(bundleStoreURL)}
	if grantStoreURL != "" {
		s.grantClient = grantclient.New(grantStoreURL, "")
	}
	return s
}

// A session started with a bundle is offered exactly the bundle's tools, by
// id, whatever the instance has opted in.
func TestInjectMCPConfigProvisionsTheBundlesTools(t *testing.T) {
	bundles, resolved := fakeBundleStore(t, "6", []int64{13, 14}, nil)
	tools, asked := fakeToolStoreWithOptIns(t, map[string][]map[string]any{"inst-1": {mcpTool12}})
	s := newServerWithBundles(tools.URL, bundles.URL, "")
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", BundleID: "6"}

	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*resolved) != 1 {
		t.Fatalf("bundle-store /resolve was asked %d times, want 1", len(*resolved))
	}
	if len(*asked) != 1 {
		t.Fatalf("tool-store /provision was asked %d times, want 1: %v", len(*asked), *asked)
	}
	ids, _ := (*asked)[0]["tool_ids"].([]any)
	if _, byInstance := (*asked)[0]["instance_id"]; byInstance || len(ids) != 2 || ids[0] != float64(13) || ids[1] != float64(14) {
		t.Fatalf("provision body = %v, want tool_ids [13 14] only (the bundle's, not the instance's opt-in 12)", (*asked)[0])
	}
	path := mcpConfigPath(t, sess)
	if path == "" {
		t.Fatal("the bundle's tools did not reach the session")
	}
	t.Cleanup(func() { os.Remove(path) })
}

// A bundle names what the work needs; a grant says what the person may have.
// With both, only the bundle tools the grants name are offered.
func TestInjectMCPConfigBundleNarrowedByGrants(t *testing.T) {
	bundles, _ := fakeBundleStore(t, "6", []int64{13, 14}, nil)
	grants := fakeGrantStore(t, "principal_000001", []string{"14", "99"})
	tools, asked := fakeToolStoreWithOptIns(t, nil)
	s := newServerWithBundles(tools.URL, bundles.URL, grants.URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", BundleID: "6", PrincipalID: "principal_000001"}

	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	ids, _ := (*asked)[0]["tool_ids"].([]any)
	if len(ids) != 1 || ids[0] != float64(14) {
		t.Fatalf("provision body = %v, want tool_ids [14] (13 is in the bundle but not granted; 99 is granted but not in the bundle)", (*asked)[0])
	}
	path := mcpConfigPath(t, sess)
	t.Cleanup(func() { os.Remove(path) })
}

// Lenient, matching the grant path: a principal holding no can_use tool
// grant is offered the bundle unchanged.
func TestInjectMCPConfigBundleWithUngrantedPrincipalIsUnchanged(t *testing.T) {
	bundles, _ := fakeBundleStore(t, "6", []int64{13}, nil)
	grants := fakeGrantStore(t, "principal_000002", nil)
	tools, asked := fakeToolStoreWithOptIns(t, nil)
	s := newServerWithBundles(tools.URL, bundles.URL, grants.URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", BundleID: "6", PrincipalID: "principal_000002"}

	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	ids, _ := (*asked)[0]["tool_ids"].([]any)
	if len(ids) != 1 || ids[0] != float64(13) {
		t.Fatalf("provision body = %v, want the bundle's tool_ids [13]", (*asked)[0])
	}
	path := mcpConfigPath(t, sess)
	t.Cleanup(func() { os.Remove(path) })
}

// A bundle that names no MCP tools is an answer, not a gap: nothing is
// provisioned and the instance's opt-ins are NOT consulted.
func TestInjectMCPConfigEmptyBundleDoesNotFallThroughToTheInstance(t *testing.T) {
	bundles, _ := fakeBundleStore(t, "6", nil, []int64{487})
	tools, asked := fakeToolStoreWithOptIns(t, map[string][]map[string]any{"inst-1": {mcpTool12}})
	s := newServerWithBundles(tools.URL, bundles.URL, "")
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", BundleID: "6", HarnessConfig: json.RawMessage(`{"settings":"{}"}`)}

	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*asked) != 0 {
		t.Fatalf("tool-store was asked %v; an empty bundle must not fall through to the instance opt-ins", *asked)
	}
	if mcpConfigPath(t, sess) != "" {
		t.Fatal("no mcp_config should be set for a bundle with no tools")
	}
}

// The caller asked for this bundle; a bundle-store that cannot answer, or a
// bundle gone since creation, stops the spawn rather than starting something
// other than what was asked for.
func TestInjectMCPConfigBundleUnresolvableAbortsTheSpawn(t *testing.T) {
	tools, asked := fakeToolStoreWithOptIns(t, map[string][]map[string]any{"inst-1": {mcpTool12}})
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for name, bundleStoreURL := range map[string]string{"down": closed.URL} {
		s := newServerWithBundles(tools.URL, bundleStoreURL, "")
		sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", BundleID: "6"}
		err := s.injectMCPConfig(sess)
		if err == nil || !strings.Contains(err.Error(), "bundle 6") {
			t.Fatalf("%s: want an error naming bundle 6, got %v", name, err)
		}
	}
	bundles, _ := fakeBundleStore(t, "6", []int64{13}, nil)
	s := newServerWithBundles(tools.URL, bundles.URL, "")
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", BundleID: "7"}
	err := s.injectMCPConfig(sess)
	if err == nil || !strings.Contains(err.Error(), "bundle 7") {
		t.Fatalf("missing: want an error naming bundle 7, got %v", err)
	}
	if len(*asked) != 0 {
		t.Fatalf("tool-store must not be asked when the bundle cannot be resolved: %v", *asked)
	}
}

// A hand-named tool list on HarnessConfig still wins over the bundle, as it
// wins over everything.
func TestInjectMCPConfigNamedToolsBeatTheBundle(t *testing.T) {
	bundles, resolved := fakeBundleStore(t, "6", []int64{13}, nil)
	srv := fakeToolStore(t, `{"mcpServers":{"x":{"command":"x"}}}`)
	s := newServerWithBundles(srv.URL, bundles.URL, "")
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", BundleID: "6", HarnessConfig: json.RawMessage(`{"tool_store_tools":["brave-search"]}`)}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*resolved) != 0 {
		t.Fatal("bundle-store must not be consulted when the caller named tools by hand")
	}
	path := mcpConfigPath(t, sess)
	t.Cleanup(func() { os.Remove(path) })
}

// bundleclient refuses a name before any call: bundle-store keys by id.
func TestBundleClientRefusesANameWithoutCalling(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	c := bundleclient.New(closed.URL)
	if err := c.CheckExists(t.Context(), "docker"); err == nil || !strings.Contains(err.Error(), "renameable") {
		t.Fatalf("want a refusal naming the reason, got %v", err)
	}
}
