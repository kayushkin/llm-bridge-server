package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	usagestore "github.com/kayushkin/usage-store"
	"github.com/kayushkin/usage-store/internal/config"
)

func serverWithSettings(t *testing.T, environment map[string]string) *Server {
	t.Helper()
	registry, err := config.NewSettingsRegistry(servicesettings.MapEnvironment(environment))
	if err != nil {
		t.Fatal(err)
	}
	store, err := usagestore.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return New(store, filepath.Join(t.TempDir(), "tokens.db"), nil, nil, nil, time.Hour, SpendCollectors{}, registry)
}

func TestGetSettingsDescribesTheServiceWithholdsTheSecretAndNothingCanBeWritten(t *testing.T) {
	const token = "a-token-that-must-never-be-served"
	srv := serverWithSettings(t, map[string]string{
		"USAGE_STORE_LIMITS_IDLE_REFRESH_INTERVAL": "20m",
		"AUTH_STORE_TOKEN":                         token,
	})

	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d: %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), token) {
		t.Fatalf("GET /settings served AUTH_STORE_TOKEN's value")
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != config.ServiceName || len(described.Settings) != len(config.SettingDefinitions()) {
		t.Fatalf("service=%q with %d settings, want %q with %d", described.Service, len(described.Settings), config.ServiceName, len(config.SettingDefinitions()))
	}
	sawInterval, sawToken := false, false
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and this service has no operator gate to put a write behind", setting.Key)
		}
		switch setting.Key {
		case config.SettingLimitsIdleRefreshInterval:
			sawInterval = true
			if setting.Value != "20m" || setting.Source != msg.ServiceSettingSourceEnvironment {
				t.Errorf("idle refresh interval served as %q from %q", setting.Value, setting.Source)
			}
		case config.SettingAuthStoreToken:
			sawToken = true
			if setting.Value != "" || setting.Source != msg.ServiceSettingSourceEnvironment {
				t.Errorf("the auth-store token served as %q from %q, want no value and the environment as its source", setting.Value, setting.Source)
			}
		}
	}
	if !sawInterval || !sawToken {
		t.Errorf("idle interval described=%v, token described=%v", sawInterval, sawToken)
	}

	recorder = httptest.NewRecorder()
	srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/settings/"+config.SettingListenAddress, strings.NewReader(`{"value":":1"}`)))
	if recorder.Code == http.StatusOK {
		t.Errorf("PUT /settings/%s = 200: a write route is mounted", config.SettingListenAddress)
	}
}

func TestNewRefusesAMissingSettingsRegistry(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New with no settings did not panic: GET /settings would fail at its first read instead of at boot")
		}
	}()
	New(nil, "", nil, nil, nil, time.Hour, SpendCollectors{}, nil)
}
