package rulestore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func TestAddressesHaveNoDefaultAndServeRequiresThem(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"HOME": "/home/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.String(SettingListenAddress); got != "" {
		t.Errorf("listen address with nothing set = %q; the port belongs in the unit", got)
	}
	err = registry.CheckRequired()
	for _, variable := range []string{"RULE_STORE_ADDR", "RULE_STORE_LLM_BRIDGE_URL", "RULE_STORE_KANBAN_STORE_URL", "LLMBRIDGE_SERVICE_TOKEN", "KANBAN_STORE_SERVICE_TOKEN"} {
		if err == nil || !strings.Contains(err.Error(), variable) {
			t.Errorf("CheckRequired = %v, want it to name %s", err, variable)
		}
	}
	if registry.Duration(SettingSweepInterval).String() != "1m0s" {
		t.Errorf("sweep interval default = %s", registry.Duration(SettingSweepInterval))
	}
}

func TestTheRegistryRefusesARuleStoreVariableNobodyDeclared(t *testing.T) {
	_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"RULE_STORE_ADDRESS": ":1"}))
	if err == nil || !strings.Contains(err.Error(), "RULE_STORE_ADDRESS") {
		t.Fatalf("NewSettingsRegistry = %v, want a refusal naming the misspelled variable", err)
	}
}

func TestGetSettingsHidesTheTokens(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"RULE_STORE_ADDR": ":9", "LLMBRIDGE_SERVICE_TOKEN": "synthetic-bridge-token", "KANBAN_STORE_SERVICE_TOKEN": "synthetic-kanban-token"}))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterSettingsHandler(mux, registry)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "synthetic-") {
		t.Fatal("GET /settings shows a token")
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and there is no operator gate", setting.Key)
		}
	}
}
