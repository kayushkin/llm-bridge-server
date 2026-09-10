package server

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
	modelstore "github.com/kayushkin/model-store"
)

// testModelStore opens a throwaway registry seeded with what model resolution
// needs to answer: a provider, two models, one alias, and the canonical
// `default` and `efficient` roles. The shared fixtures hand it to New so every
// test server has a registry — the server's contract now requires one, and a
// nil store fails resolution loudly by design (see model_selection.go).
func testModelStore(t *testing.T) *modelstore.Store {
	t.Helper()
	mds, err := modelstore.Open(filepath.Join(t.TempDir(), "models.db"))
	if err != nil {
		t.Fatalf("open model-store: %v", err)
	}
	t.Cleanup(func() { mds.Close() })
	if err := mds.AddProvider(modelstore.Provider{ID: "mock", Name: "Mock"}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	for _, id := range []string{"mock-model", "mock-model-alt"} {
		if err := mds.AddModel(modelstore.Model{ID: id, Provider: "mock", Name: id, MaxTokens: 1000, Enabled: true}); err != nil {
			t.Fatalf("seed model %s: %v", id, err)
		}
	}
	if err := mds.AddAlias("mock-model-alt", "alt"); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	// "opus" mirrors the real registry, where it is an alias for the current Opus id.
	if err := mds.AddAlias("mock-model", "opus"); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	if err := mds.SetRole(modelstore.RoleDefault, "mock-model"); err != nil {
		t.Fatalf("seed default role: %v", err)
	}
	if err := mds.SetRole(modelstore.RoleEfficient, "mock-model-alt"); err != nil {
		t.Fatalf("seed efficient role: %v", err)
	}
	return mds
}

func sessionWithConfig(harness, cfg string) *store.Session {
	return &store.Session{SessionID: "s_test", Harness: msg.Harness(harness), HarnessConfig: json.RawMessage(cfg)}
}

// pinnedModel reads the two keys the resolver writes out of a session row.
func pinnedModel(t *testing.T, raw json.RawMessage) (string, msg.ModelSelection) {
	t.Helper()
	var cfg struct {
		Model     string             `json:"model"`
		Selection msg.ModelSelection `json:"model_selection"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("harness_config is not an object: %v (%s)", err, raw)
	}
	return cfg.Model, cfg.Selection
}

func TestResolveModelSelectionSessionOverrideWins(t *testing.T) {
	srv, _ := testServer(t)
	sel, err := srv.resolveModelSelection(sessionWithConfig("mock", `{"model":"mock-model-alt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Model != "mock-model-alt" || sel.SelectedBy != msg.ModelSelectedBySession || sel.Role != "" {
		t.Fatalf("selection = %+v; want mock-model-alt selected by session with no role", sel)
	}
}

func TestResolveModelSelectionRoleNameOverrideRecordsTheRole(t *testing.T) {
	srv, _ := testServer(t)
	sel, err := srv.resolveModelSelection(sessionWithConfig("mock", `{"model":"efficient"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Model != "mock-model-alt" || sel.Role != msg.ModelRoleEfficient || sel.SelectedBy != msg.ModelSelectedBySession {
		t.Fatalf("selection = %+v; want the efficient role resolved to mock-model-alt, selected by session", sel)
	}
}

func TestResolveModelSelectionAliasOverrideResolves(t *testing.T) {
	srv, _ := testServer(t)
	sel, err := srv.resolveModelSelection(sessionWithConfig("mock", `{"model":"alt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Model != "mock-model-alt" {
		t.Fatalf("alias resolved to %q, want mock-model-alt", sel.Model)
	}
}

func TestResolveModelSelectionPrefsBeatRegistryDefault(t *testing.T) {
	srv, _ := testServer(t)
	srv.bridgePrefs.mu.Lock()
	srv.bridgePrefs.data.Defaults = map[string]msg.HarnessDefaults{"mock": {Model: "mock-model-alt"}}
	srv.bridgePrefs.mu.Unlock()
	sel, err := srv.resolveModelSelection(sessionWithConfig("mock", ``))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Model != "mock-model-alt" || sel.SelectedBy != msg.ModelSelectedByPrefs {
		t.Fatalf("selection = %+v; want mock-model-alt selected by prefs", sel)
	}
}

func TestResolveModelSelectionRegistryDefaultIsTheFloor(t *testing.T) {
	srv, _ := testServer(t)
	sel, err := srv.resolveModelSelection(sessionWithConfig("mock", ``))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Model != "mock-model" || sel.Role != msg.ModelRoleDefault || sel.SelectedBy != msg.ModelSelectedByRole {
		t.Fatalf("selection = %+v; want the registry default role", sel)
	}
}

func TestResolveModelSelectionUnknownModelFailsLoud(t *testing.T) {
	srv, _ := testServer(t)
	_, err := srv.resolveModelSelection(sessionWithConfig("mock", `{"model":"claude-imaginary"}`))
	if err == nil || !strings.Contains(err.Error(), "the registry knows") {
		t.Fatalf("err = %v; want a refusal naming the unknown model", err)
	}
}

func TestResolveModelSelectionUnassignedDefaultRoleFailsLoud(t *testing.T) {
	srv, _ := testServer(t)
	if err := srv.modelStore.RemoveRole(modelstore.RoleDefault); err != nil {
		t.Fatal(err)
	}
	_, err := srv.resolveModelSelection(sessionWithConfig("mock", ``))
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("err = %v; want a refusal naming the unassigned default role — never a fabricated fallback", err)
	}
}

func TestResolveModelSelectionWithoutRegistryFailsLoud(t *testing.T) {
	srv, _ := testServer(t)
	srv.modelStore = nil
	_, err := srv.resolveModelSelection(sessionWithConfig("mock", `{"model":"mock-model"}`))
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v; want a refusal: the registry is the only source of a model", err)
	}
}

func TestResolveModelSelectionUnparseableConfigIsRefused(t *testing.T) {
	srv, _ := testServer(t)
	if _, err := srv.resolveModelSelection(sessionWithConfig("mock", `not json`)); err == nil {
		t.Fatal("an unreadable harness_config resolved to something; want a refusal — it may hold the override the caller asked for")
	}
}

func createRequestWithConfig(instID, sessionID, cfg string) msg.CreateSessionRequest {
	return msg.CreateSessionRequest{
		Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "test",
		Harness: msg.HarnessMock, InstanceID: instID, SessionID: sessionID,
		HarnessConfig: json.RawMessage(cfg),
	}
}

func TestCreateSessionPinsTheResolvedModelIntoTheRow(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)
	got := postCreateSession(t, srv, createRequestWithConfig(instID, "", ``))
	row, err := st.GetSession(got.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	model, sel := pinnedModel(t, row.HarnessConfig)
	if model != "mock-model" || sel.Model != "mock-model" || sel.SelectedBy != msg.ModelSelectedByRole || sel.Role != msg.ModelRoleDefault {
		t.Fatalf("row pinned model=%q selection=%+v; want the registry default, attributed to the role layer", model, sel)
	}
}

func TestCreateSessionWithUnresolvableModelIs422AndNoRow(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)
	resp := doJSON(t, srv, "POST", "/sessions", createRequestWithConfig(instID, "br_unresolvable", `{"model":"claude-imaginary"}`))
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 422 || !strings.Contains(string(body), "model_unresolvable") {
		t.Fatalf("status=%d body=%s; want 422 model_unresolvable", resp.StatusCode, body)
	}
	if _, err := st.GetSession("br_unresolvable"); err == nil {
		t.Fatal("a session row was created for a model nothing can run")
	}
}

func TestConfigResolvesAndPersistsTheModelWithoutALiveProcess(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)
	got := postCreateSession(t, srv, createRequestWithConfig(instID, "", ``))
	resp := doJSON(t, srv, "POST", "/sessions/"+got.SessionID+"/config", msg.ConfigSessionRequest{Model: "efficient"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("config status = %d, want 200", resp.StatusCode)
	}
	row, err := st.GetSession(got.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	model, sel := pinnedModel(t, row.HarnessConfig)
	if model != "mock-model-alt" || sel.Role != msg.ModelRoleEfficient || sel.SelectedBy != msg.ModelSelectedBySession {
		t.Fatalf("after config: model=%q selection=%+v; want the efficient role resolved to mock-model-alt and PERSISTED", model, sel)
	}
}

func TestConfigWithUnknownModelIs400AndLeavesTheRowAlone(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)
	got := postCreateSession(t, srv, createRequestWithConfig(instID, "", ``))
	resp := doJSON(t, srv, "POST", "/sessions/"+got.SessionID+"/config", msg.ConfigSessionRequest{Model: "claude-imaginary"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(body), "model_unresolvable") {
		t.Fatalf("status=%d body=%s; want 400 model_unresolvable", resp.StatusCode, body)
	}
	row, _ := st.GetSession(got.SessionID)
	if model, _ := pinnedModel(t, row.HarnessConfig); model != "mock-model" {
		t.Fatalf("row model = %q after a refused config; want the original mock-model", model)
	}
}

func TestForkCarriesTheParentHarnessConfig(t *testing.T) {
	pathWithNoHarnessBinary(t) // the fork's spawn fails; the row it creates is what this pins
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)
	// Fork refuses a parent with no harness_session_id (409), and only a session
	// that has actually run has one — so the parent is seeded as a row that ran,
	// carrying the model the user chose, rather than created through the handler.
	parent := &store.Session{
		SessionID: "br_parent", Harness: msg.HarnessMock, InstanceID: instID,
		HarnessSessionID: "11111111-2222-3333-4444-555555555555",
		State:            string(msg.SessionIdle), Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "test",
		HarnessConfig: json.RawMessage(`{"model":"mock-model-alt"}`),
	}
	if err := st.CreateSession(parent); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	resp := doJSON(t, srv, "POST", "/sessions/br_parent/fork", map[string]any{"display_name": "fork", "type": "interactive"})
	defer resp.Body.Close()
	var forked store.Session
	if err := json.NewDecoder(resp.Body).Decode(&forked); err != nil {
		t.Fatalf("fork status=%d, decode: %v", resp.StatusCode, err)
	}
	row, err := st.GetSession(forked.SessionID)
	if err != nil {
		t.Fatalf("forked row: %v", err)
	}
	if model, _ := pinnedModel(t, row.HarnessConfig); model != "mock-model-alt" {
		t.Fatalf("fork pinned model = %q; want the parent's mock-model-alt — a fork used to lose it entirely", model)
	}
}
