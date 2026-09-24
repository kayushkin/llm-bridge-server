package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/bundleclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// bundleStoreDenying is a bundle-store that knows bundle 6 and resolves it to
// the given denied tool ids and denied read paths, and nothing else.
func bundleStoreDenying(t *testing.T, deniedToolIDs []int64, deniedReadPaths []string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bundles/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "6" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 6, "name": "support-read-only", "enabled": true})
	})
	mux.HandleFunc("POST /resolve", func(w http.ResponseWriter, r *http.Request) {
		denied := []map[string]any{}
		for _, id := range deniedToolIDs {
			denied = append(denied, map[string]any{"id": id, "name": "denied"})
		}
		if deniedReadPaths == nil {
			deniedReadPaths = []string{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bundles": []map[string]any{{"id": 6, "name": "support-read-only"}},
			"skills":  []any{}, "tools": []any{},
			"denied_tools": denied, "denied_read_paths": deniedReadPaths,
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

func serverWithBundleAndToolStore(t *testing.T, bundleStoreURL string) (*Server, *store.Store, string) {
	t.Helper()
	srv, st, instanceID := serverWithHarnessDefaults(t, msg.HarnessDefaults{})
	tools, _ := fakeToolStoreWithOptIns(t, nil)
	srv.cfg.ToolStoreURL = tools.URL
	srv.bundleClient = bundleclient.New(bundleStoreURL)
	return srv, st, instanceID
}

func createSessionExpectingRefusal(t *testing.T, srv *Server, request msg.CreateSessionRequest) string {
	t.Helper()
	request.Type, request.Purpose, request.Origin = msg.SessionTypeAutonomous, msg.PurposeChat, "test"
	if request.Harness == "" {
		request.Harness = msg.HarnessClaudeCode
	}
	resp := doJSON(t, srv, "POST", "/sessions", request)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusCreated {
		t.Fatalf("POST /sessions = 201; want a refusal (%s)", body)
	}
	return string(body)
}

// A bundle's denied harness tool is off, and a creator who sends their own
// (even empty) list cannot give it back.
func TestABundlesDeniedHarnessToolIsDisabledWhateverTheCreatorSends(t *testing.T) {
	srv, st, instanceID := serverWithBundleAndToolStore(t, bundleStoreDenying(t, []int64{102}, nil))
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6", HarnessConfig: json.RawMessage(`{"disabled_tools":[]}`)})
	pinned := pinnedOn(t, sess)
	if strings.Join(pinned.DisabledTools, ",") != "Bash" {
		t.Errorf("disabled_tools = %v; want the bundle's Bash despite the creator's empty list", pinned.DisabledTools)
	}
	if pinned.Layers[msg.EffectiveSettingDisabledTools] != msg.EffectiveLayerSession {
		t.Errorf("layer = %q; the creator sent a list, so the narrowest contributor is the session", pinned.Layers[msg.EffectiveSettingDisabledTools])
	}
	setting := effectiveSettingOf(t, srv, sess.SessionID, msg.EffectiveSettingDisabledTools)
	if !strings.Contains(strings.Join(setting.Notes, "|"), "bundle-store bundle 6 denies: [Bash]") {
		t.Errorf("effective config notes %v do not name the bundle's deny", setting.Notes)
	}
}

// A denied tool of another harness does not reach this harness, and a denied
// id that is not a harness tool (an MCP tool) is not a built-in to disable.
func TestADeniedToolOfAnotherHarnessOrAnMCPToolIsNotABuiltInToDisable(t *testing.T) {
	srv, st, instanceID := serverWithBundleAndToolStore(t, bundleStoreDenying(t, []int64{103, 13}, nil))
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})
	if got := pinnedOn(t, sess).DisabledTools; len(got) != 0 {
		t.Errorf("disabled_tools = %v; want none for a claude_code session", got)
	}
}

// tool-store's master switch turns a harness tool off in every session.
func TestAHarnessToolDisabledInToolStoreIsOffInEverySession(t *testing.T) {
	saved := harnessToolRowsForTests
	t.Cleanup(func() { harnessToolRowsForTests = saved })
	harnessToolRowsForTests = []map[string]any{
		{"id": 101, "name": "claude_code.Read", "kind": "harness", "harness": "claude_code", "harness_tool_name": "Read", "enabled": true},
		{"id": 104, "name": "claude_code.WebSearch", "kind": "harness", "harness": "claude_code", "harness_tool_name": "WebSearch", "enabled": false},
	}
	srv, st, instanceID := serverWithBundleAndToolStore(t, bundleStoreDenying(t, nil, nil))
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID})
	pinned := pinnedOn(t, sess)
	if strings.Join(pinned.DisabledTools, ",") != "WebSearch" || pinned.Layers[msg.EffectiveSettingDisabledTools] != msg.EffectiveLayerToolStore {
		t.Errorf("disabled_tools = %v by %q; want WebSearch by tool_store", pinned.DisabledTools, pinned.Layers[msg.EffectiveSettingDisabledTools])
	}
}

// A bundle that denies reading paths but leaves a command-running tool on is
// refused: a command reads around a Read deny rule.
func TestDeniedReadPathsWithACommandRunningToolOnAreRefused(t *testing.T) {
	srv, _, instanceID := serverWithBundleAndToolStore(t, bundleStoreDenying(t, nil, []string{"~/.config/**"}))
	body := createSessionExpectingRefusal(t, srv, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})
	if !strings.Contains(body, "leaves [Bash] on") {
		t.Errorf("refusal %q does not name Bash", body)
	}
}

// Only claude_code enforces denied read paths; codex reads through its shell.
func TestDeniedReadPathsOnACodexSessionAreRefused(t *testing.T) {
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessCodex)
	tools, _ := fakeToolStoreWithOptIns(t, nil)
	srv.cfg.ToolStoreURL = tools.URL
	srv.bundleClient = bundleclient.New(bundleStoreDenying(t, []int64{103}, []string{"/proc/**"}))
	body := createSessionExpectingRefusal(t, srv, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6", Harness: msg.HarnessCodex})
	if !strings.Contains(body, "only claude_code enforces") {
		t.Errorf("refusal %q does not say codex cannot enforce it", body)
	}
}

// Denied read paths reach Claude Code as Read deny rules, absolute paths with
// the double slash Claude Code needs to treat them as absolute.
func TestDeniedReadPathsBecomeClaudeCodeReadDenyRules(t *testing.T) {
	srv, st, instanceID := serverWithBundleAndToolStore(t, bundleStoreDenying(t, []int64{102}, []string{"~/.config/**", "/proc/**"}))
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})
	raw, err := srv.buildClaudeCodeSettings(sess)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("settings %q: %v", raw, err)
	}
	if got := strings.Join(settings.Permissions.Deny, " "); got != "Read(~/.config/**) Read(//proc/**)" {
		t.Errorf("deny rules = %q", got)
	}
	setting := effectiveSettingOf(t, srv, sess.SessionID, msg.EffectiveSettingDeniedReadPaths)
	if setting.Layer != msg.EffectiveLayerBundle {
		t.Errorf("effective denied_read_paths layer = %q", setting.Layer)
	}
}

// A caller's own settings would drop the deny rules, so such a spawn fails.
func TestACallersOwnSettingsCannotDropTheDenyRules(t *testing.T) {
	srv, _ := testServer(t)
	sess := &store.Session{SessionID: "br_own_settings", Harness: msg.HarnessClaudeCode,
		HarnessConfig: json.RawMessage(`{"settings":"{}","denied_read_paths":["~/.ssh/**"]}`)}
	if err := srv.injectHookSettings(sess); err == nil {
		t.Fatal("injectHookSettings accepted a caller's settings on a session with denied read paths")
	}
}

// A harness tool the bundle includes is never sent to tool-store's
// /provision, which provisions MCP servers only.
func TestABundlesIncludedHarnessToolIsNotProvisioned(t *testing.T) {
	bundles, _ := fakeBundleStore(t, "6", []int64{101, 13}, nil)
	tools, provisioned := fakeToolStoreWithOptIns(t, nil)
	s := newServerWithBundles(tools.URL, bundles.URL, "")
	sess := &store.Session{Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", BundleID: "6"}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*provisioned) != 1 {
		t.Fatalf("/provision called %d times", len(*provisioned))
	}
	if ids := (*provisioned)[0]["tool_ids"].([]any); len(ids) != 1 || ids[0] != float64(13) {
		t.Errorf("/provision got tool_ids %v; want only the MCP tool 13", ids)
	}
}

// POST /sessions/{id}/config replaces the creator's list, never the bundle's
// denials.
func TestAConfigChangeCannotGiveBackABundlesDeniedTool(t *testing.T) {
	srv, st, instanceID := serverWithBundleAndToolStore(t, bundleStoreDenying(t, []int64{102}, nil))
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})
	resp := doJSON(t, srv, "POST", "/sessions/"+sess.SessionID+"/config", msg.ConfigSessionRequest{DisabledTools: []string{"WebFetch"}})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST config = %d: %s", resp.StatusCode, body)
	}
	stored, err := st.GetSession(sess.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pinnedOn(t, stored).DisabledTools, ","); got != "WebFetch,Bash" {
		t.Errorf("disabled_tools after the change = %q; want the new list plus the bundle's Bash", got)
	}
}
