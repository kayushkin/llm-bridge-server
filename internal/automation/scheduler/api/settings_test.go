package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db/dbtest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/cron"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
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

// A setting read where the process is started is refused a write; only the
// stored model roles take one.
func TestSettingsRefusesAWriteToASettingItDoesNotStore(t *testing.T) {
	server := setup(t, "")
	request := httptest.NewRequest(http.MethodPut, "/settings/port", strings.NewReader(`{"value":"1"}`))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("PUT /settings/port = %d, want 409: %s", recorder.Code, recorder.Body)
	}
}

// storedSettingsServer is the API over a registry whose model roles are stored
// in the API's own database and checked by validate, as cmd/scheduler wires
// it.
func storedSettingsServer(t *testing.T, validate func(string) error) *Server {
	t.Helper()
	store := dbtest.NewStore(t)
	registry := settingsRegistryForTests(t, "")
	if err := registry.AttachStoredValues(store.ServiceSettingValues(), nil); err != nil {
		t.Fatal(err)
	}
	for _, key := range config.ModelRoleSettingKeys {
		registry.SetValidator(key, validate)
	}
	return New(store, cron.New(store), "", registry, checkModelRoleForTests)
}

func putSetting(server *Server, key, value string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPut, "/settings/"+key, strings.NewReader(`{"value":"`+value+`"}`))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func describedSetting(t *testing.T, server *Server, key string) msg.ServiceSetting {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings", nil))
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	for _, setting := range described.Settings {
		if setting.Key == key {
			return setting
		}
	}
	t.Fatalf("GET /settings does not describe %s", key)
	return msg.ServiceSetting{}
}

func TestAModelRoleIsStoredByPutAndServedByGet(t *testing.T) {
	var checked []string
	server := storedSettingsServer(t, func(role string) error { checked = append(checked, role); return nil })

	seeded := describedSetting(t, server, config.SettingKanbanScoperModelRole)
	if seeded.Value != config.DefaultKanbanScoperModelRole || seeded.Source != msg.ServiceSettingSourceStored || !seeded.Editable {
		t.Fatalf("before any PUT = %+v, want the default, stored and editable", seeded)
	}

	if recorder := putSetting(server, config.SettingKanbanScoperModelRole, "balanced"); recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", recorder.Code, recorder.Body)
	}
	if got := describedSetting(t, server, config.SettingKanbanScoperModelRole); got.Value != "balanced" {
		t.Errorf("after PUT = %+v, want balanced", got)
	}
	if len(checked) != 1 || checked[0] != "balanced" {
		t.Errorf("the validator saw %v, want [balanced]", checked)
	}
}

func TestAModelRoleTheValidatorRefusesIsNotStored(t *testing.T) {
	server := storedSettingsServer(t, func(role string) error { return errors.New("model-store has no role " + role) })
	if recorder := putSetting(server, config.SettingKanbanScoperModelRole, "cheapest"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT an unknown role = %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if recorder := putSetting(server, config.SettingKanbanScoperModelRole, ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT an empty role = %d, want 400 (the setting is required): %s", recorder.Code, recorder.Body)
	}
	if got := describedSetting(t, server, config.SettingKanbanScoperModelRole); got.Value != config.DefaultKanbanScoperModelRole {
		t.Errorf("after refused PUTs = %q, want the default untouched", got.Value)
	}
}

func TestAModelRoleIsNotStoredWhileModelStoreCannotBeAsked(t *testing.T) {
	server := storedSettingsServer(t, func(string) error {
		return fmt.Errorf("%w: model-store: connection refused", servicesettings.ErrSettingOwnerUnavailable)
	})
	if recorder := putSetting(server, config.SettingKanbanScoperModelRole, "balanced"); recorder.Code != http.StatusBadGateway {
		t.Fatalf("PUT with model-store down = %d, want 502: %s", recorder.Code, recorder.Body)
	}
}

func TestNewRefusesANilSettingsRegistry(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("api.New with a nil registry did not panic")
		}
	}()
	New(nil, nil, "", nil, checkModelRoleForTests)
}

func TestNewRefusesANilModelRoleChecker(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("api.New with no model-role checker did not panic")
		}
	}()
	New(nil, nil, "", settingsRegistryForTests(t, ""), nil)
}
