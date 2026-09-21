package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/slava/scheduler/internal/config"
)

// settingsRegistryForTests builds the service's registry with token as
// SCHEDULER_TOKEN and a NOTEBOARD_URL no other test uses, so a /settings body
// can be checked for both.
func settingsRegistryForTests(t *testing.T, token string) *servicesettings.Registry {
	t.Helper()
	variables := map[string]string{"NOTEBOARD_URL": "http://noteboard.test:1"}
	if token != "" {
		variables["SCHEDULER_TOKEN"] = token
	}
	registry, err := config.NewSettingsRegistry(servicesettings.MapEnvironment(variables))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestSettingsAnswersAtTheRootBehindTheBearerAndWithholdsTheToken(t *testing.T) {
	const token = "scheduler-settings-test-token-not-used-elsewhere"
	server := setup(t, token)

	request := httptest.NewRequest(http.MethodGet, "/settings", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("GET /settings with no bearer = %d, want 401", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/settings", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /settings with the bearer = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	if strings.Contains(body, token) {
		t.Fatal("GET /settings carries the value of SCHEDULER_TOKEN")
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != config.ServiceName {
		t.Errorf("service = %q, want %q", described.Service, config.ServiceName)
	}
	values := map[string]msg.ServiceSetting{}
	for _, setting := range described.Settings {
		values[setting.Key] = setting
	}
	// The value the registry was built with, not the default: a route that
	// described a registry nobody built would show http://localhost:8191.
	if got := values[config.SettingNoteboardURL]; got.Value != "http://noteboard.test:1" || got.Source != msg.ServiceSettingSourceEnvironment {
		t.Errorf("noteboard_url = %+v, want the environment's value", got)
	}
	if got := values[config.SettingToken]; !got.IsSet || got.Value != "" {
		t.Errorf("token = %+v, want set and withheld", got)
	}
}

// Nothing is Editable, so no write is mounted.
func TestSettingsTakesNoWrite(t *testing.T) {
	server := setup(t, "")
	request := httptest.NewRequest(http.MethodPut, "/settings/port", strings.NewReader(`{"value":"1"}`))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusOK {
		t.Fatalf("PUT /settings/port = 200: %s", recorder.Body)
	}
	for _, definition := range config.SettingDefinitions() {
		if definition.Editable {
			t.Errorf("%s is Editable, and the service stores no setting", definition.EnvironmentVariable)
		}
	}
}

func TestNewRefusesANilSettingsRegistry(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("api.New with a nil registry did not panic")
		}
	}()
	New(nil, nil, "", nil)
}
