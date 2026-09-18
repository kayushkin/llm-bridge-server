package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/bundleclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// bundleStoreNaming is a bundle-store that knows bundle 6 and resolves it to
// the given model and effort, with no tools or skills.
func bundleStoreNaming(t *testing.T, model, effort string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bundles/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "6" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 6, "name": "reviewer", "enabled": true})
	})
	mux.HandleFunc("POST /resolve", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bundles": []map[string]any{{"id": 6, "name": "reviewer"}},
			"model":   model, "effort": effort,
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

func serverWithHarnessDefaults(t *testing.T, defaults msg.HarnessDefaults) (*Server, *store.Store, string) {
	t.Helper()
	srv, st, instanceID := testServerWithInstance(t, msg.HarnessClaudeCode)
	srv.bridgePrefs.mu.Lock()
	srv.bridgePrefs.data.Defaults = map[string]msg.HarnessDefaults{string(msg.HarnessClaudeCode): defaults}
	srv.bridgePrefs.mu.Unlock()
	return srv, st, instanceID
}

func createSessionForDefaults(t *testing.T, srv *Server, st *store.Store, request msg.CreateSessionRequest) *store.Session {
	t.Helper()
	request.Type, request.Purpose, request.Origin, request.Harness = msg.SessionTypeAutonomous, msg.PurposeChat, "test", msg.HarnessClaudeCode
	resp := doJSON(t, srv, "POST", "/sessions", request)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /sessions = %d: %s", resp.StatusCode, body)
	}
	created := decodeJSON[msg.ManagedSession](t, resp)
	stored, err := st.GetSession(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

type pinnedSessionSettings struct {
	Model         string              `json:"model"`
	Selection     msg.ModelSelection  `json:"model_selection"`
	Effort        string              `json:"effort"`
	DisabledTools []string            `json:"disabled_tools"`
	Layers        pinnedSettingLayers `json:"setting_layers"`
}

func pinnedOn(t *testing.T, sess *store.Session) pinnedSessionSettings {
	t.Helper()
	var pinned pinnedSessionSettings
	if err := json.Unmarshal(sess.HarnessConfig, &pinned); err != nil {
		t.Fatalf("harness_config: %v (%s)", err, sess.HarnessConfig)
	}
	return pinned
}

func effectiveSettingOf(t *testing.T, srv *Server, sessionID string, key msg.EffectiveSettingKey) msg.EffectiveSetting {
	t.Helper()
	resp := doJSON(t, srv, "GET", "/sessions/"+sessionID+"/effective-config", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("effective-config = %d: %s", resp.StatusCode, body)
	}
	for _, setting := range decodeJSON[msg.EffectiveConfig](t, resp).Settings {
		if setting.Key == key {
			return setting
		}
	}
	t.Fatalf("no %s in the effective config", key)
	return msg.EffectiveSetting{}
}

var dispatcherDefaults = msg.HarnessDefaults{Model: "mock-model", Effort: "high", MaxBudget: float64Ptr(7), DisabledTools: []string{"WebSearch"}}

func TestACreatorWhoNamesNothingGetsTheHarnessDefaultsPinnedByTheServer(t *testing.T) {
	srv, st, instanceID := serverWithHarnessDefaults(t, dispatcherDefaults)
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID})
	pinned := pinnedOn(t, sess)

	if pinned.Model != "mock-model" || pinned.Selection.SelectedBy != msg.ModelSelectedByPrefs {
		t.Errorf("model = %q selected by %q", pinned.Model, pinned.Selection.SelectedBy)
	}
	if pinned.Effort != "high" || sess.MaxBudgetUSD != 7 || len(pinned.DisabledTools) != 1 || pinned.DisabledTools[0] != "WebSearch" {
		t.Errorf("effort=%q max_budget=%v disabled_tools=%v; the server did not apply the harness defaults", pinned.Effort, sess.MaxBudgetUSD, pinned.DisabledTools)
	}
	for _, key := range []msg.EffectiveSettingKey{msg.EffectiveSettingEffort, msg.EffectiveSettingMaxBudget, msg.EffectiveSettingDisabledTools} {
		if pinned.Layers[key] != msg.EffectiveLayerHarnessDefault {
			t.Errorf("pinned layer of %s = %q", key, pinned.Layers[key])
		}
		if layer := effectiveSettingOf(t, srv, sess.SessionID, key).Layer; layer != msg.EffectiveLayerHarnessDefault {
			t.Errorf("effective config reports %s as %q", key, layer)
		}
	}
}

func TestTheBundleOutranksTheHarnessDefaultForModelAndEffort(t *testing.T) {
	srv, st, instanceID := serverWithHarnessDefaults(t, dispatcherDefaults)
	srv.bundleClient = bundleclient.New(bundleStoreNaming(t, "alt", "low"))
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID, BundleID: "6"})
	pinned := pinnedOn(t, sess)

	if pinned.Model != "mock-model-alt" || pinned.Selection.SelectedBy != msg.ModelSelectedByBundle {
		t.Errorf("model = %q selected by %q; want the bundle's alias resolved through the registry", pinned.Model, pinned.Selection.SelectedBy)
	}
	if pinned.Effort != "low" || pinned.Layers[msg.EffectiveSettingEffort] != msg.EffectiveLayerBundle {
		t.Errorf("effort = %q by %q; want the bundle's", pinned.Effort, pinned.Layers[msg.EffectiveSettingEffort])
	}
	// A bundle names no ceiling and no tools to disable, so the harness default still decides those.
	if sess.MaxBudgetUSD != 7 || pinned.Layers[msg.EffectiveSettingMaxBudget] != msg.EffectiveLayerHarnessDefault {
		t.Errorf("max_budget = %v by %q", sess.MaxBudgetUSD, pinned.Layers[msg.EffectiveSettingMaxBudget])
	}
	if model := effectiveSettingOf(t, srv, sess.SessionID, msg.EffectiveSettingModel); model.Layer != msg.EffectiveLayerBundle {
		t.Errorf("effective config reports the model as %q", model.Layer)
	}
	if effort := effectiveSettingOf(t, srv, sess.SessionID, msg.EffectiveSettingEffort); effort.Layer != msg.EffectiveLayerBundle || effort.Value != "low" {
		t.Errorf("effective config reports effort %v as %q", effort.Value, effort.Layer)
	}
}

func TestTheSessionsOwnValueOutranksTheBundleAndTheDefaultEvenWhenItIsZeroOrEmpty(t *testing.T) {
	srv, st, instanceID := serverWithHarnessDefaults(t, dispatcherDefaults)
	srv.bundleClient = bundleclient.New(bundleStoreNaming(t, "alt", "low"))
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{
		InstanceID: instanceID, BundleID: "6", MaxBudget: float64Ptr(0),
		HarnessConfig: json.RawMessage(`{"model":"mock-model","effort":"medium","disabled_tools":[]}`),
	})
	pinned := pinnedOn(t, sess)

	if pinned.Model != "mock-model" || pinned.Selection.SelectedBy != msg.ModelSelectedBySession || pinned.Effort != "medium" {
		t.Errorf("model=%q by %q effort=%q; the creator's own values lost", pinned.Model, pinned.Selection.SelectedBy, pinned.Effort)
	}
	if sess.MaxBudgetUSD != 0 {
		t.Errorf("max_budget = %v; a sent 0 means no ceiling and must not become the default's 7", sess.MaxBudgetUSD)
	}
	if pinned.DisabledTools == nil || len(pinned.DisabledTools) != 0 {
		t.Errorf("disabled_tools = %v; a sent empty list means disable nothing", pinned.DisabledTools)
	}
	for _, key := range []msg.EffectiveSettingKey{msg.EffectiveSettingEffort, msg.EffectiveSettingMaxBudget, msg.EffectiveSettingDisabledTools} {
		if pinned.Layers[key] != msg.EffectiveLayerSession {
			t.Errorf("layer of %s = %q, want session", key, pinned.Layers[key])
		}
	}
}

func TestAPinnedSettingDoesNotMoveWhenTheDefaultChangesAndBecomesTheSessionsOwnWhenChanged(t *testing.T) {
	srv, st, instanceID := serverWithHarnessDefaults(t, dispatcherDefaults)
	sess := createSessionForDefaults(t, srv, st, msg.CreateSessionRequest{InstanceID: instanceID})

	srv.bridgePrefs.mu.Lock()
	srv.bridgePrefs.data.Defaults = map[string]msg.HarnessDefaults{string(msg.HarnessClaudeCode): {Model: "mock-model-alt", Effort: "low", MaxBudget: float64Ptr(99)}}
	srv.bridgePrefs.mu.Unlock()
	if effort := effectiveSettingOf(t, srv, sess.SessionID, msg.EffectiveSettingEffort); effort.Value != "high" || effort.Layer != msg.EffectiveLayerHarnessDefault {
		t.Errorf("effort after the default changed = %v by %q; want the pinned high", effort.Value, effort.Layer)
	}

	resp := doJSON(t, srv, "POST", "/sessions/"+sess.SessionID+"/config", map[string]any{"effort": "medium", "max_budget": 3})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /config = %d: %s", resp.StatusCode, body)
	}
	if effort := effectiveSettingOf(t, srv, sess.SessionID, msg.EffectiveSettingEffort); effort.Value != "medium" || effort.Layer != msg.EffectiveLayerSession {
		t.Errorf("effort after POST /config = %v by %q", effort.Value, effort.Layer)
	}
	if ceiling := effectiveSettingOf(t, srv, sess.SessionID, msg.EffectiveSettingMaxBudget); ceiling.Layer != msg.EffectiveLayerSession {
		t.Errorf("max_budget after POST /config is reported as %q", ceiling.Layer)
	}
	if tools := effectiveSettingOf(t, srv, sess.SessionID, msg.EffectiveSettingDisabledTools); tools.Layer != msg.EffectiveLayerHarnessDefault {
		t.Errorf("disabled_tools was not touched and is reported as %q", tools.Layer)
	}
}

func TestADryRunReportsWhatCreateWouldPinForADispatcher(t *testing.T) {
	srv, _, instanceID := serverWithHarnessDefaults(t, dispatcherDefaults)
	srv.bundleClient = bundleclient.New(bundleStoreNaming(t, "", "low"))
	resp := doJSON(t, srv, "GET", "/effective-config?instance_id="+instanceID+"&bundle_id=6", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dry run = %d: %s", resp.StatusCode, body)
	}
	layers := map[msg.EffectiveSettingKey]msg.EffectiveConfigLayer{}
	for _, setting := range decodeJSON[msg.EffectiveConfig](t, resp).Settings {
		layers[setting.Key] = setting.Layer
	}
	// The bundle names an effort and no model, so each falls to a different layer.
	if layers[msg.EffectiveSettingEffort] != msg.EffectiveLayerBundle || layers[msg.EffectiveSettingModel] != msg.EffectiveLayerHarnessDefault || layers[msg.EffectiveSettingMaxBudget] != msg.EffectiveLayerHarnessDefault {
		t.Errorf("dry run layers = %v", layers)
	}
}
