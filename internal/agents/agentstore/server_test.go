package agentstore

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// NewTestServer creates a test HTTP server backed by an in-memory store.
func NewTestServer(t *testing.T) (*Store, *http.ServeMux) {
	t.Helper()
	s := testStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	return s, mux
}

func TestHTTPAgentsCRUD(t *testing.T) {
	_, mux := NewTestServer(t)

	// Create agent
	body := `{"slug":"test","display_name":"Test","enabled":true}`
	req := httptest.NewRequest("POST", "/agents", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// List agents
	req = httptest.NewRequest("GET", "/agents", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list: expected 200, got %d", w.Code)
	}
	var agents []Agent
	json.NewDecoder(w.Body).Decode(&agents)
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}

	// Get agent
	req = httptest.NewRequest("GET", "/agents/test", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}

	// Update agent
	body = `{"display_name":"Test Updated","enabled":true}`
	req = httptest.NewRequest("PUT", "/agents/test", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("update: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Delete agent
	req = httptest.NewRequest("DELETE", "/agents/test", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("delete: expected 200, got %d", w.Code)
	}

	// Confirm deleted
	req = httptest.NewRequest("GET", "/agents/test", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404 after delete, got %d", w.Code)
	}
}

func TestHTTPReconcile(t *testing.T) {
	_, mux := NewTestServer(t)

	req := httptest.NewRequest("GET", "/reconcile", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("reconcile: expected 200, got %d", w.Code)
	}
}

func TestHTTPAgentConfig(t *testing.T) {
	s, mux := NewTestServer(t)

	a := &Agent{Slug: "cfg-agent", DisplayName: "Cfg Agent", Role: "test role", Enabled: true}
	s.UpsertAgent(a)
	s.UpsertHarness(&Harness{ID: "inber", DisplayName: "Inber", DefaultAgentID: &a.ID})
	s.UpsertAgentHarness(&AgentHarness{
		AgentID: a.ID, HarnessID: "inber", HarnessAgentID: "cfg-agent",
		Enabled: true, ModelPrimary: "claude-sonnet-4-5", ThinkingBudget: 1024,
		ContextBudget: 30000, ContextTags: `["code"]`, MaxTurns: 3,
	})
	s.SetAgentTools(a.ID, "inber", []string{"shell", "read_file"})
	s.UpsertAgentNature(&AgentNature{AgentID: a.ID, Kind: "identity", Content: "test soul"})

	// GET /agents/cfg-agent/config?harness=inber
	req := httptest.NewRequest("GET", "/agents/cfg-agent/config?harness=inber", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var cfg FullAgentConfig
	json.NewDecoder(w.Body).Decode(&cfg)
	if cfg.Model != "claude-sonnet-4-5" {
		t.Fatalf("expected model claude-sonnet-4-5, got %s", cfg.Model)
	}
	if len(cfg.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(cfg.Tools))
	}
	if cfg.Soul != "test soul" {
		t.Fatalf("expected soul, got %q", cfg.Soul)
	}

	// GET /configs?harness=inber
	req = httptest.NewRequest("GET", "/configs?harness=inber", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["default"] != "cfg-agent" {
		t.Fatalf("expected default cfg-agent, got %v", resp["default"])
	}
}

// TestHTTPHealth pins the standalone shape: a process that owns its mux
// (cmd/server) registers the domain handlers plus its own /health.
func TestHTTPHealth(t *testing.T) {
	s := testStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	RegisterHealthHandler(mux)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("health: expected 200, got %d", w.Code)
	}
}

// TestRegisterHandlersEmbedsInHostOwningHealth pins the contract that makes
// agent-store safe to mount as a library: RegisterHandlers must not claim any
// pattern the host already owns.
//
// This is a regression test for a real outage-in-waiting. agent-store briefly
// registered "GET /health" from RegisterHandlers; llm-bridge-server mounts
// agent-store into its *root* mux and serves its own "GET /health", and Go
// 1.22+ ServeMux panics on a conflicting pattern at registration time. The
// gateway's next deploy would have built a binary that panicked at boot.
//
// Nothing caught it: agent-store compiled, its own tests passed (they never
// simulated a host), and the gateway's tests construct the server with a nil
// agent-store so its routes were never mounted under test. The panic was only
// reachable from a real boot. Registering into a mux that already owns /health
// -- exactly what the gateway does -- is what makes it reachable from `go test`.
func TestRegisterHandlersEmbedsInHostOwningHealth(t *testing.T) {
	s := testStore(t)

	// A host mux that already serves its own /health, like llm-bridge-server.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "host"})
	})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RegisterHandlers panicked when embedded in a host mux: %v\n\n"+
				"agent-store is mounted as a library into host muxes (llm-bridge-server "+
				"mounts it at the root). Every pattern it registers must live under a "+
				"segment agent-store owns -- /agents, /configs, /reconcile, /files, "+
				"/versions, /seed, /context. A generic top-level route panics the host "+
				"at boot. If you meant to add a process-level route, it belongs in "+
				"cmd/server, not in RegisterHandlers.", r)
		}
	}()
	RegisterHandlers(mux, s)

	// The host's own /health must still be the one that answers.
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("host health: expected 200, got %d", w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode host health: %v", err)
	}
	if resp["status"] != "host" {
		t.Fatalf("host health: agent-store shadowed the host's handler, got status=%q want %q",
			resp["status"], "host")
	}
}

// --- Wire-shape regression tests -------------------------------------------
//
// The write path used to decode into structs with NO json tags. encoding/json
// matches Go field names case-insensitively but does NOT ignore underscores, so
// every snake_case multi-word field was silently discarded and the route still
// answered 201/200. TestHTTPAgentsCRUD above did not catch it: it POSTs a
// snake_case display_name already, but only ever asserted the status code and
// the agent count -- never the value that was being thrown away.
//
// Each test below fails against the untagged structs.

// do issues a request and returns the recorder.
func do(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestCreateAgentKeepsSnakeCaseDisplayName(t *testing.T) {
	_, mux := NewTestServer(t)

	w := do(t, mux, "POST", "/agents", `{"slug":"snake","display_name":"Ogham","role":"scribe"}`)
	if w.Code != 201 {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var got Agent
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DisplayName != "Ogham" {
		t.Fatalf("display_name was dropped on write: got %q, want %q", got.DisplayName, "Ogham")
	}
	if got.Role != "scribe" {
		t.Fatalf("role was dropped on write: got %q", got.Role)
	}

	// And it must actually be in the store, not merely echoed back.
	w = do(t, mux, "GET", "/agents/snake", "")
	if w.Code != 200 {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}
	var fetched Agent
	json.NewDecoder(w.Body).Decode(&fetched)
	if fetched.DisplayName != "Ogham" {
		t.Fatalf("display_name did not persist: got %q", fetched.DisplayName)
	}
}

// TestUpdateAgentKeepsOmittedFields pins the bug that was actively destroying
// live data: bridge-ui's Agents page sends {slug, display_name, emoji, projects,
// enabled} when you tick the Enabled checkbox -- no `role`, no `description`.
// PUT decoded that into a BLANK Agent and wrote every column, so one click
// erased the agent's display name (via the snake_case drop), its role, and its
// description. All three must survive.
func TestUpdateAgentKeepsOmittedFields(t *testing.T) {
	_, mux := NewTestServer(t)

	w := do(t, mux, "POST", "/agents",
		`{"slug":"argraphments","display_name":"Ogham","emoji":"X","projects":"argraphments","description":"keeps notes","role":"scribe","enabled":true}`)
	if w.Code != 201 {
		t.Fatalf("seed: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// The exact body bridge-ui's toggleEnabled() sends.
	w = do(t, mux, "PUT", "/agents/argraphments",
		`{"slug":"argraphments","display_name":"Ogham","emoji":"X","projects":"argraphments","enabled":false}`)
	if w.Code != 200 {
		t.Fatalf("toggle: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = do(t, mux, "GET", "/agents/argraphments", "")
	var got Agent
	json.NewDecoder(w.Body).Decode(&got)

	if got.Enabled {
		t.Fatalf("the toggle itself did not take effect")
	}
	if got.DisplayName != "Ogham" {
		t.Fatalf("toggling Enabled wiped display_name: got %q", got.DisplayName)
	}
	if got.Role != "scribe" {
		t.Fatalf("toggling Enabled wiped role (the client never sent it): got %q", got.Role)
	}
	if got.Description != "keeps notes" {
		t.Fatalf("toggling Enabled wiped description (the client never sent it): got %q", got.Description)
	}
}

// A field the client sends explicitly as "" must still clear -- the merge above
// changes the meaning of OMISSION, and must not cost anyone the ability to blank
// a field on purpose.
func TestUpdateAgentStillClearsFieldsSentEmpty(t *testing.T) {
	_, mux := NewTestServer(t)

	do(t, mux, "POST", "/agents", `{"slug":"a","display_name":"Name","role":"scribe"}`)
	w := do(t, mux, "PUT", "/agents/a", `{"slug":"a","role":""}`)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	w = do(t, mux, "GET", "/agents/a", "")
	var got Agent
	json.NewDecoder(w.Body).Decode(&got)
	if got.Role != "" {
		t.Fatalf("an explicit empty role must clear the field, got %q", got.Role)
	}
	if got.DisplayName != "Name" {
		t.Fatalf("display_name was omitted, so it must survive: got %q", got.DisplayName)
	}
}

// An unmapped field must FAIL LOUDLY. A 201 that silently discarded part of the
// body is what let the original defect live: the client believed it wrote a
// display name, and the store agreed, and neither was true.
func TestWriteRoutesRejectUnknownFields(t *testing.T) {
	_, mux := NewTestServer(t)
	do(t, mux, "POST", "/agents", `{"slug":"known"}`)

	cases := []struct{ name, method, path, body string }{
		{"POST /agents", "POST", "/agents", `{"slug":"x","displayName":"camelCase is not the wire shape"}`},
		{"PUT /agents/{slug}", "PUT", "/agents/known", `{"slug":"known","nonsense":"x"}`},
		{"POST harnesses", "POST", "/agents/known/harnesses", `{"harness_id":"inber","bogus_field":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, mux, c.method, c.path, c.body)
			if w.Code != 400 {
				t.Fatalf("an unknown field must be rejected, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// AgentHarness carried the same untagged defect, and worse -- nearly every field
// is multi-word, so a snake_case client lost almost the entire harness config.
func TestCreateHarnessKeepsSnakeCaseFields(t *testing.T) {
	s, mux := NewTestServer(t)
	if err := s.UpsertHarness(&Harness{ID: "inber", DisplayName: "Inber"}); err != nil {
		t.Fatalf("seed harness: %v", err) // agent_harness.harness_id is a FK
	}
	do(t, mux, "POST", "/agents", `{"slug":"a","display_name":"A"}`)

	w := do(t, mux, "POST", "/agents/a/harnesses",
		`{"harness_id":"inber","model_primary":"claude-opus-4-8","max_turns":7,"is_default":true,"system_prompt":"be brief","enabled":true}`)
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var got AgentHarness
	json.NewDecoder(w.Body).Decode(&got)

	if got.HarnessID != "inber" {
		t.Fatalf("harness_id dropped: got %q", got.HarnessID)
	}
	if got.ModelPrimary != "claude-opus-4-8" {
		t.Fatalf("model_primary dropped: got %q", got.ModelPrimary)
	}
	if got.MaxTurns != 7 {
		t.Fatalf("max_turns dropped: got %d", got.MaxTurns)
	}
	if !got.IsDefault {
		t.Fatalf("is_default dropped")
	}
	if got.SystemPrompt != "be brief" {
		t.Fatalf("system_prompt dropped: got %q", got.SystemPrompt)
	}
}
