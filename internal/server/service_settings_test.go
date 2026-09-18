package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
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
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessClaudeCode)
	// The Config literal names no classifier instance, so the seed is empty
	// and the classifier's one-shot refuses, as it did before settings were stored.
	if _, err := srv.classifierOneShot(t.Context(), msg.OneShotRequest{}); err == nil || !strings.Contains(err.Error(), "no signal-classifier instance") {
		t.Fatalf("classifierOneShot with no instance stored = %v", err)
	}

	put := func(key, value string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(msg.ServiceSettingUpdate{Value: value})
		return serve(srv, serviceSettingsRequest("PUT", "/settings/"+key, string(body), true))
	}
	if got := put(config.SettingSignalClassifierInstance, "inst-nobody-registered"); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "knows no instance") {
		t.Fatalf("an unknown instance = %d %s, want 400 naming it", got.Code, got.Body.String())
	}
	accepted := put(config.SettingSignalClassifierInstance, instanceID)
	if accepted.Code != http.StatusOK {
		t.Fatalf("a known instance = %d %s", accepted.Code, accepted.Body.String())
	}
	if setting := describedSetting(t, accepted, config.SettingSignalClassifierInstance); setting.Value != instanceID || setting.Source != msg.ServiceSettingSourceStored || !setting.Editable {
		t.Errorf("described after the write: %+v", setting)
	}
	if got := srv.settings.String(config.SettingSignalClassifierInstance); got != instanceID {
		t.Errorf("the value read at use = %q", got)
	}
}

func TestAChangedClassifierSettingIsInForceWithoutARestart(t *testing.T) {
	srv, _ := testServer(t)
	if srv.signalClassifier.enabledFor(msg.HarnessClaudeCode) {
		t.Fatal("the classifier is on with no model in the Config literal")
	}
	for key, value := range map[string]string{
		config.SettingSignalClassifierModel:             "claude-haiku-4-5",
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
	if tuning.model != "claude-haiku-4-5" || tuning.timeout != 45*time.Second || tuning.maxChars != 900 {
		t.Errorf("classifier tuning = %+v", tuning)
	}
	if !srv.signalClassifier.enabledFor(msg.HarnessClaudeCode) || srv.signalClassifier.enabledFor(msg.HarnessCodex) {
		t.Error("the classifier should now run for claude_code and skip codex")
	}
	if model, timeout := srv.questionTriage.current(); model != "claude-haiku-4-5" || timeout != 45*time.Second {
		t.Errorf("triage did not follow: %q %v", model, timeout)
	}
	if got := serve(srv, serviceSettingsRequest("PUT", "/settings/"+config.SettingSignalClassifierTimeout, `{"value":"soon"}`, true)); got.Code != http.StatusBadRequest {
		t.Errorf("a malformed duration = %d, want 400", got.Code)
	}
	if got := serve(srv, serviceSettingsRequest("PUT", "/settings/listen_address", `{"value":":1"}`, true)); got.Code != http.StatusConflict {
		t.Errorf("a wiring setting = %d, want 409", got.Code)
	}
}
