package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/bundleclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// observingToolStore is a tool-store that serves its harness tools and, like
// the real one, registers a reported name it has never seen as a harness tool
// switched off and tagged unreviewed.
type observingToolStore struct {
	mu   sync.Mutex
	rows []map[string]any
	URL  string
}

func newObservingToolStore(t *testing.T, rows []map[string]any) *observingToolStore {
	t.Helper()
	fake := &observingToolStore{rows: rows}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		matching := []map[string]any{}
		for _, row := range fake.rows {
			if harness := r.URL.Query().Get("harness"); harness == "" || row["harness"] == harness {
				matching = append(matching, row)
			}
		}
		_ = json.NewEncoder(w).Encode(matching)
	})
	mux.HandleFunc("POST /harness-tools/observed", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Harness   string   `json:"harness"`
			ToolNames []string `json:"tool_names"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		known := map[string]bool{}
		for _, row := range fake.rows {
			if row["harness"] == body.Harness {
				known[row["harness_tool_name"].(string)] = true
			}
		}
		created := []string{}
		for _, name := range body.ToolNames {
			if !known[name] {
				fake.rows = append(fake.rows, map[string]any{"id": 900 + len(fake.rows), "name": body.Harness + "." + name, "kind": "harness",
					"harness": body.Harness, "harness_tool_name": name, "enabled": false, "tags": []string{"unreviewed"}})
				created = append(created, name)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"created": created, "seen": len(body.ToolNames)})
	})
	mux.HandleFunc("GET /instances/{id}/tools", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{})
	})
	mux.HandleFunc("POST /provision", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"mcpServers": map[string]any{}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	fake.URL = server.URL
	return fake
}

// reviewedClaudeCodeTools is tool-store as a person left it: Read reviewed and
// on, Bash on but command-running, and SendToProject seen once and still
// unreviewed, so off.
func reviewedClaudeCodeTools() []map[string]any {
	return []map[string]any{
		{"id": 101, "name": "claude_code.Read", "kind": "harness", "harness": "claude_code", "harness_tool_name": "Read", "enabled": true, "tags": []string{"read-only"}},
		{"id": 102, "name": "claude_code.Bash", "kind": "harness", "harness": "claude_code", "harness_tool_name": "Bash", "enabled": true, "tags": []string{"effects", "runs-commands"}},
		{"id": 105, "name": "claude_code.SendToProject", "kind": "harness", "harness": "claude_code", "harness_tool_name": "SendToProject", "enabled": false, "tags": []string{"unreviewed"}},
	}
}

type pinnedAllowList struct {
	Tools         *[]string `json:"tools"`
	PinnedFor     string    `json:"built_in_tool_allow_list_pinned_for_bundle"`
	DisabledTools []string  `json:"disabled_tools"`
}

func allowListPinnedOn(t *testing.T, sess *store.Session) pinnedAllowList {
	t.Helper()
	var pinned pinnedAllowList
	if err := json.Unmarshal(sess.HarnessConfig, &pinned); err != nil {
		t.Fatalf("harness_config: %v (%s)", err, sess.HarnessConfig)
	}
	return pinned
}

func restrictedServer(t *testing.T, toolStoreURL string, deniedToolIDs []int64, deniedReadPaths []string) (*Server, *store.Store, string) {
	t.Helper()
	srv, st, instanceID := serverWithHarnessDefaults(t, msg.HarnessDefaults{})
	srv.cfg.ToolStoreURL = toolStoreURL
	srv.bundleClient = bundleclient.New(bundleStoreDenying(t, deniedToolIDs, deniedReadPaths))
	return srv, st, instanceID
}

// The regression for the first-session gap. A tool this Claude Code has and
// tool-store has never seen (NewWriteTool, standing for one that writes, runs
// commands or sends) is not on a restricted session's allow list, so Claude
// Code is never offered it, even in the session that first reports it. When
// that session reports it anyway, the harness disobeyed --tools, and the
// session is stopped. The next restricted session finds it registered off.
func TestANewlyObservedToolIsNotOfferedInItsFirstRestrictedSession(t *testing.T) {
	toolStore := newObservingToolStore(t, reviewedClaudeCodeTools())
	srv, st, instanceID := restrictedServer(t, toolStore.URL, []int64{102}, []string{"~/.config/**"})

	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})
	pinned := allowListPinnedOn(t, sess)
	if pinned.Tools == nil || strings.Join(*pinned.Tools, ",") != "Read" {
		t.Fatalf("tools = %v; want only the reviewed Read, with Bash denied and SendToProject unreviewed", pinned.Tools)
	}
	if pinned.PinnedFor != "6" {
		t.Errorf("allow list pinned for bundle %q; want 6", pinned.PinnedFor)
	}

	srv.onSessionInfo(sess.SessionID, &msg.SessionInfo{Tools: []msg.ToolInfo{{Name: "Read"}, {Name: "NewWriteTool"}}})

	stored, err := st.GetSession(sess.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(msg.SessionAborted) {
		t.Errorf("state = %q; a restricted session reporting a tool off its list must be stopped", stored.State)
	}

	second := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})
	secondPinned := allowListPinnedOn(t, second)
	if strings.Join(*secondPinned.Tools, ",") != "Read" {
		t.Errorf("second session tools = %v; want only Read", *secondPinned.Tools)
	}
	if !strings.Contains(strings.Join(secondPinned.DisabledTools, ","), "NewWriteTool") {
		t.Errorf("second session disabled_tools = %v; tool-store registered NewWriteTool off", secondPinned.DisabledTools)
	}
}

// The normal case: a reviewed read tool is offered, and a session that reports
// only what it was allowed goes on.
func TestAReviewedReadToolIsOfferedAndTheSessionGoesOn(t *testing.T) {
	toolStore := newObservingToolStore(t, reviewedClaudeCodeTools())
	srv, st, instanceID := restrictedServer(t, toolStore.URL, []int64{102}, []string{"~/.config/**"})
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})

	srv.onSessionInfo(sess.SessionID, &msg.SessionInfo{Tools: []msg.ToolInfo{{Name: "Read"}, {Name: "mcp__ticketing__get_ticket"}}})

	stored, err := st.GetSession(sess.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State == string(msg.SessionAborted) {
		t.Errorf("a session that reported only its allowed Read (and an MCP tool) was stopped")
	}
	setting := effectiveSettingOf(t, srv, sess.SessionID, msg.EffectiveSettingDisabledTools)
	if !strings.Contains(strings.Join(setting.Notes, "|"), "offered only these built-in tools") {
		t.Errorf("effective config notes %v do not show the allow list", setting.Notes)
	}
}

// A bundle that denies every enabled tool pins an empty list, which Claude
// Code reads as no built-in tool, never a missing one, which would be all.
func TestARestrictedSessionWithNothingLeftIsOfferedNoBuiltInTool(t *testing.T) {
	toolStore := newObservingToolStore(t, reviewedClaudeCodeTools())
	srv, st, instanceID := restrictedServer(t, toolStore.URL, []int64{101, 102}, nil)
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})
	if !strings.Contains(string(sess.HarnessConfig), `"tools":[]`) {
		t.Errorf("harness_config = %s; want tools pinned as []", sess.HarnessConfig)
	}
}

// A session with no bundle, or one that denies nothing, keeps Claude Code's
// default set: the allow list is for restricted sessions only.
func TestAnUnrestrictedSessionGetsNoAllowList(t *testing.T) {
	toolStore := newObservingToolStore(t, reviewedClaudeCodeTools())
	srv, st, instanceID := restrictedServer(t, toolStore.URL, nil, nil)
	for _, bundleID := range []string{"", "6"} {
		sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: bundleID})
		if pinned := allowListPinnedOn(t, sess); pinned.Tools != nil || pinned.PinnedFor != "" {
			t.Errorf("bundle %q: harness_config = %s; want no allow list", bundleID, sess.HarnessConfig)
		}
		srv.onSessionInfo(sess.SessionID, &msg.SessionInfo{Tools: []msg.ToolInfo{{Name: "Read"}, {Name: "NewWriteTool"}}})
		if stored, _ := st.GetSession(sess.SessionID); stored.State == string(msg.SessionAborted) {
			t.Errorf("bundle %q: an unrestricted session was stopped for a new tool", bundleID)
		}
	}
}

// A caller's own tools would replace the list tool-store decides.
func TestARestrictedSessionRefusesTheCallersOwnTools(t *testing.T) {
	toolStore := newObservingToolStore(t, reviewedClaudeCodeTools())
	srv, _, instanceID := restrictedServer(t, toolStore.URL, []int64{102}, nil)
	body := createSessionExpectingRefusal(t, srv, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6", HarnessConfig: json.RawMessage(`{"tools":["Read","NewWriteTool"]}`)})
	if !strings.Contains(body, "built_in_tool_allow_list_unenforceable") {
		t.Errorf("refusal %q does not name the allow list", body)
	}
}

// codex takes no allow list, so a restricted codex session is refused rather
// than offered a tool nobody reviewed.
func TestARestrictedCodexSessionIsRefused(t *testing.T) {
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessCodex)
	toolStore := newObservingToolStore(t, reviewedClaudeCodeTools())
	srv.cfg.ToolStoreURL = toolStore.URL
	srv.bundleClient = bundleclient.New(bundleStoreDenying(t, []int64{102}, nil))
	body := createSessionExpectingRefusal(t, srv, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6", Harness: msg.HarnessCodex})
	if !strings.Contains(body, "built_in_tool_allow_list_unenforceable") || !strings.Contains(body, "only claude_code") {
		t.Errorf("refusal %q does not say codex cannot be held to an allow list", body)
	}
}

// With tool-store down nothing can say which tools are on, so the session is
// refused, under a code that names tool-store.
func TestARestrictedSessionIsRefusedVisiblyWhenToolStoreIsDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	srv, _, instanceID := restrictedServer(t, downURL, []int64{102}, []string{"~/.config/**"})
	body := createSessionExpectingRefusal(t, srv, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})
	if !strings.Contains(body, "tool_store_unavailable") {
		t.Errorf("refusal %q does not name tool-store", body)
	}
}
