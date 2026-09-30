package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func serviceSettingsRequest(method, path, body string, asOperator bool) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if asOperator {
		request.Header.Set(serviceTokenHeader, testServiceToken)
	}
	return request
}

func describedSetting(t *testing.T, recorder *httptest.ResponseRecorder, key string) msg.ServiceSetting {
	t.Helper()
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	for _, setting := range described.Settings {
		if setting.Key == key {
			return setting
		}
	}
	t.Fatalf("no setting %s described", key)
	return msg.ServiceSetting{}
}

func TestSettingsAreAnOperatorRouteAndNeverCarryASecret(t *testing.T) {
	srv, _ := testServer(t)
	if got := serve(srv, serviceSettingsRequest("GET", "/settings", "", false)); got.Code != http.StatusUnauthorized {
		t.Fatalf("GET /settings with no credentials = %d, want 401", got.Code)
	}
	if got := serve(srv, serviceSettingsRequest("PUT", "/settings/"+config.SettingSessionIdleTimeout, `{"value":"1m"}`, false)); got.Code != http.StatusUnauthorized {
		t.Fatalf("PUT /settings/{key} with no credentials = %d, want 401", got.Code)
	}
	listed := serve(srv, serviceSettingsRequest("GET", "/settings", "", true))
	if listed.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d %s", listed.Code, listed.Body.String())
	}
	if strings.Contains(listed.Body.String(), testServiceToken) || strings.Contains(listed.Body.String(), testSigningKey) {
		t.Fatal("the settings description carries a secret's value")
	}
	if setting := describedSetting(t, listed, "listen_address"); setting.Editable || setting.Kind != msg.ServiceSettingKindWiring {
		t.Errorf("listen_address: editable=%v kind=%s", setting.Editable, setting.Kind)
	}
}

func TestAStoredSettingIsSeededFromTheConfigAndReadWhenItIsUsed(t *testing.T) {
	srv, _, _ := testServerWithInstance(t, msg.HarnessClaudeCode)
	// The Config literal names no classifier role, so the seed is empty and
	// a routed call with it refuses.
	if role := srv.settings.ModelRole(config.SettingSignalClassifierModelRole); role != "" {
		t.Fatalf("seeded classifier role = %q, want empty", role)
	}
	if _, err := srv.routedOneShotJSON(t.Context(), msg.OneShotRequest{Prompt: "hi"}); err == nil || !strings.Contains(err.Error(), "no model role") {
		t.Fatalf("a routed call with no role = %v", err)
	}

	put := func(key, value string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(msg.ServiceSettingUpdate{Value: value})
		return serve(srv, serviceSettingsRequest("PUT", "/settings/"+key, string(body), true))
	}
	if got := put(config.SettingSignalClassifierModelRole, "cheapest"); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "not a model role") {
		t.Fatalf("an unknown role = %d %s, want 400 naming it", got.Code, got.Body.String())
	}
	accepted := put(config.SettingSignalClassifierModelRole, "efficient")
	if accepted.Code != http.StatusOK {
		t.Fatalf("a known role = %d %s", accepted.Code, accepted.Body.String())
	}
	setting := describedSetting(t, accepted, config.SettingSignalClassifierModelRole)
	if setting.Value != "efficient" || setting.Source != msg.ServiceSettingSourceStored || !setting.Editable || setting.ValueType != msg.ServiceSettingValueTypeModelRole {
		t.Errorf("described after the write: %+v", setting)
	}
	if got := srv.settings.ModelRole(config.SettingSignalClassifierModelRole); got != "efficient" {
		t.Errorf("the value read at use = %q", got)
	}
}

// The stored rows of the retired model and instance keys are gone after a
// start, and the provider-to-instance map keeps its value under its new key.
func TestStoredSettingsMoveOffTheRetiredKeysAtStart(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for key, value := range map[string]string{
		"operations.completion_instances": "anthropic:inst-a,openai:inst-b",
		"signal_classifier.model":         "gpt-5.6-terra",
		"signal_classifier.instance":      "inst-b",
		"session_actions.review_model":    "balanced",
		"operations.completion_instance":  "inst-a",
	} {
		if err := st.ServiceSettingValues().Save(key, value); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{ImagesDir: t.TempDir(), BridgePrefsPath: filepath.Join(t.TempDir(), "prefs.json"), LogStoreURL: "http://localhost:0"}
	testAuthorizationConfig(cfg)
	srv := New(st, nil, nil, nil, nil, nil, nil, cfg)
	held, err := st.ServiceSettingValues().Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range config.RetiredStoredSettingKeys {
		if value, still := held[retired]; still {
			t.Errorf("retired key %s still holds %q", retired, value)
		}
	}
	if got := held[config.SettingOneShotInstanceByProvider]; got != "anthropic:inst-a,openai:inst-b" {
		t.Errorf("oneshot.instance_by_provider = %q, want the old completion_instances value", got)
	}
	if got := srv.settings.StringMap(config.SettingOneShotInstanceByProvider)["openai"]; got != "inst-b" {
		t.Errorf("in force for openai = %q", got)
	}
	if got := held[config.SettingSessionActionsReviewModelRole]; got != "balanced" {
		t.Errorf("review role = %q, want the declared default", got)
	}
}

func TestAChangedClassifierSettingIsInForceWithoutARestart(t *testing.T) {
	srv, _ := testServer(t)
	if srv.signalClassifier.enabledFor(msg.HarnessClaudeCode) {
		t.Fatal("the classifier is on with no model role in the Config literal")
	}
	for key, value := range map[string]string{
		config.SettingSignalClassifierModelRole:         "efficient",
		config.SettingSignalClassifierTimeout:           "45s",
		config.SettingSignalClassifierMaximumCharacters: "900",
		config.SettingSignalClassifierOptOutHarnesses:   "codex",
	} {
		body, _ := json.Marshal(msg.ServiceSettingUpdate{Value: value})
		if got := serve(srv, serviceSettingsRequest("PUT", "/settings/"+key, string(body), true)); got.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", key, got.Code, got.Body.String())
		}
	}
	tuning := srv.signalClassifier.currentTuning()
	if tuning.modelRole != "efficient" || tuning.timeout != 45*time.Second || tuning.maxChars != 900 {
		t.Errorf("classifier tuning = %+v", tuning)
	}
	if !srv.signalClassifier.enabledFor(msg.HarnessClaudeCode) || srv.signalClassifier.enabledFor(msg.HarnessCodex) {
		t.Error("the classifier should now run for claude_code and skip codex")
	}
	if modelRole, timeout := srv.questionTriage.current(); modelRole != "efficient" || timeout != 45*time.Second {
		t.Errorf("triage did not follow: %q %v", modelRole, timeout)
	}
	if got := serve(srv, serviceSettingsRequest("PUT", "/settings/"+config.SettingSignalClassifierTimeout, `{"value":"soon"}`, true)); got.Code != http.StatusBadRequest {
		t.Errorf("a malformed duration = %d, want 400", got.Code)
	}
	if got := serve(srv, serviceSettingsRequest("PUT", "/settings/listen_address", `{"value":":1"}`, true)); got.Code != http.StatusConflict {
		t.Errorf("a wiring setting = %d, want 409", got.Code)
	}
}
