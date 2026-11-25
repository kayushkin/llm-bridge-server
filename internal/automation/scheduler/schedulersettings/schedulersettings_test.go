package schedulersettings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func serveSettings(t *testing.T, settings ...msg.ServiceSetting) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/settings" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(msg.ServiceSettings{Service: "scheduler", Settings: settings})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestReadModelRoleReturnsTheStoredRole(t *testing.T) {
	server := serveSettings(t,
		msg.ServiceSetting{Key: "port", ValueType: msg.ServiceSettingValueTypeInteger, Value: "8092"},
		msg.ServiceSetting{Key: "kanban_scoper_model_role", ValueType: msg.ServiceSettingValueTypeModelRole, Value: "efficient"},
	)
	role, err := ReadModelRole(context.Background(), server.URL+"/", "kanban_scoper_model_role")
	if err != nil {
		t.Fatal(err)
	}
	if role != "efficient" {
		t.Errorf("role = %q, want efficient", role)
	}
}

func TestReadModelRoleRefusesWhatItCannotUse(t *testing.T) {
	for name, testCase := range map[string]struct {
		setting msg.ServiceSetting
		want    string
	}{
		"missing":      {msg.ServiceSetting{Key: "other", ValueType: msg.ServiceSettingValueTypeModelRole, Value: "best"}, "declares no such setting"},
		"wrong type":   {msg.ServiceSetting{Key: "wanted", ValueType: msg.ServiceSettingValueTypeString, Value: "best"}, "not model_role"},
		"no role held": {msg.ServiceSetting{Key: "wanted", ValueType: msg.ServiceSettingValueTypeModelRole}, "holds no role"},
	} {
		server := serveSettings(t, testCase.setting)
		if _, err := ReadModelRole(context.Background(), server.URL, "wanted"); err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, testCase.want)
		}
	}
}

func TestReadModelRoleRefusesAFailingScheduler(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	if _, err := ReadModelRole(context.Background(), server.URL, "wanted"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want the scheduler's status", err)
	}
	if _, err := ReadModelRole(context.Background(), "", "wanted"); err == nil {
		t.Error("an empty scheduler URL was accepted")
	}
}
