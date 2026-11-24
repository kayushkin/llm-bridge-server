package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/kanban-store/internal/api"
	"github.com/kayushkin/kanban-store/internal/config"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// What the settings tests start the registry with. The token values are ones
// no other test uses, so finding one in a response body means /settings leaked
// it.
const (
	settingsTestListenPort             = "18305"
	settingsTestServiceToken           = "a-kanban-store-service-token-that-settings-must-withhold"
	settingsTestGrantStoreServiceToken = "a-grant-store-service-token-that-settings-must-withhold"
	settingsTestFileStoreServiceToken  = "a-file-store-service-token-that-settings-must-withhold"
	settingsTestAuthStoreToken         = "an-auth-store-token-that-settings-must-withhold"
)

// settingsRegistryForTests is the registry every test API is built with:
// api.New takes no nil.
func settingsRegistryForTests(t *testing.T) *servicesettings.Registry {
	t.Helper()
	registry, err := config.NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"KANBAN_PORT":                settingsTestListenPort,
		"KANBAN_DB":                  "/srv/kanban-store.db",
		"KANBAN_STORE_SERVICE_TOKEN": settingsTestServiceToken,
		"GRANT_STORE_URL":            "http://grants.example:1",
		"GRANT_STORE_SERVICE_TOKEN":  settingsTestGrantStoreServiceToken,
		"FILE_STORE_URL":             "http://files.example:1",
		"FILE_STORE_SERVICE_TOKEN":   settingsTestFileStoreServiceToken,
		"AUTH_STORE_TOKEN":           settingsTestAuthStoreToken,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// GET /settings is the operator's: the service token and an administrator read
// it, a member holding a board does not, and nobody is 401 as everywhere else.
func TestSettingsAnswerTheServiceTokenAndAnAdministratorOnly(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	grants.give(alice, "can_administer", "board-that-does-not-matter")

	mustStatus(t, requestAs(t, h, nil, "GET", "/settings", nil), 401, "no caller")
	mustStatus(t, requestAs(t, h, map[string]string{api.ServiceTokenHeader: strings.Repeat("x", 40)}, "GET", "/settings", nil), 401, "a wrong service token")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/settings", nil), 403, "a member who administers a board")
	mustStatus(t, requestAs(t, h, asService, "GET", "/settings", nil), 200, "the service token")
	mustStatus(t, requestAs(t, h, asPrincipal(deploymentAdministrator), "GET", "/settings", nil), 200, "an administrator")
}

// The body is servicesettings' description of this service: the value in force
// and where it came from, and for a secret only whether it is set.
func TestSettingsDescribeTheServiceAndWithholdEverySecret(t *testing.T) {
	h, _, _ := setupWithPrincipalEnforcement(t)
	response := requestAs(t, h, asService, "GET", "/settings", nil)
	mustStatus(t, response, 200, "GET /settings")

	body := response.Body.String()
	for _, secret := range []string{settingsTestServiceToken, settingsTestGrantStoreServiceToken, settingsTestFileStoreServiceToken, settingsTestAuthStoreToken} {
		if strings.Contains(body, secret) {
			t.Errorf("GET /settings carries the secret %q", secret)
		}
	}

	var described msg.ServiceSettings
	if err := json.Unmarshal(response.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != config.ServiceName {
		t.Errorf("service = %q, want %q", described.Service, config.ServiceName)
	}
	if got, want := len(described.Settings), len(config.SettingDefinitions()); got != want {
		t.Fatalf("%d settings described, %d declared", got, want)
	}
	byKey := map[string]msg.ServiceSetting{}
	for _, setting := range described.Settings {
		byKey[setting.Key] = setting
		if setting.Editable {
			t.Errorf("%s is described as editable, and PUT /settings/{key} is not mounted", setting.Key)
		}
	}
	if port := byKey[config.SettingListenPort]; port.Value != settingsTestListenPort || port.Source != msg.ServiceSettingSourceEnvironment {
		t.Errorf("listen_port = %q from %q, want %s from the environment", port.Value, port.Source, settingsTestListenPort)
	}
	if noteboard := byKey[config.SettingNoteboardURL]; noteboard.Value != config.DefaultNoteboardURL || noteboard.Source != msg.ServiceSettingSourceDefault {
		t.Errorf("noteboard_url = %q from %q, want the default", noteboard.Value, noteboard.Source)
	}
}

// Nothing is Editable, so there is no write route: a PUT reaches no handler
// that could store a value.
func TestSettingsHaveNoWriteRoute(t *testing.T) {
	h, _, _ := setupWithPrincipalEnforcement(t)
	response := requestAs(t, h, asService, "PUT", "/settings/"+config.SettingNoteboardURL, map[string]string{"value": "http://elsewhere.example"})
	if response.Code != http.StatusNotFound {
		t.Errorf("PUT /settings/{key} = %d, want 404 from the router", response.Code)
	}
	mustStatus(t, requestAs(t, h, asService, "PUT", "/settings", map[string]string{"value": "x"}), http.StatusMethodNotAllowed, "PUT /settings")
}

// A command that forgot the registry must not start and serve boards with no
// GET /settings.
func TestNewRefusesANilSettingsRegistry(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("api.New took a nil settings registry")
		}
	}()
	api.New(nil, nil, nil, nil, nil, nil)
}
