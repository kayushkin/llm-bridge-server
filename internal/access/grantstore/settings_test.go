package grantstore

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The registry gives the command what its nine environment reads gave it
// before 2026-09-20: the loopback address, the default data directory and the
// five owners' default addresses with nothing set, and the operator's values
// when they are.
func TestTheRegistryReadsTheSameValuesTheCommandAlwaysDid(t *testing.T) {
	unset, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"HOME": "/home/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		SettingListenAddress:              "127.0.0.1:8315",
		SettingDataDirectory:              DefaultDataDir(),
		SettingServiceToken:               "",
		SettingPrincipalStoreURL:          "http://127.0.0.1:8314",
		SettingLLMBridgeServerURL:         "http://127.0.0.1:8160",
		SettingSkillStoreURL:              "http://127.0.0.1:8301",
		SettingToolStoreURL:               "http://127.0.0.1:8302",
		SettingKanbanStoreURL:             "http://127.0.0.1:8305",
		SettingKanbanStoreServiceToken:    "",
		SettingLLMBridgeServiceToken:      "",
		SettingReferenceStoreURL:          "http://127.0.0.1:8343",
		SettingReferenceStoreServiceToken: "",
	} {
		if got := unset.String(key); got != want {
			t.Errorf("%s with nothing set = %q, want %q", key, got, want)
		}
	}

	operatorValues := map[string]string{
		"GRANT_STORE_ADDR":              "127.0.0.1:9999",
		"GRANT_STORE_DATA_DIR":          "/srv/grants",
		"GRANT_STORE_SERVICE_TOKEN":     enforcementTestServiceToken,
		"PRINCIPAL_STORE_URL":           "http://principals.example",
		"LLM_BRIDGE_URL":                "http://bridge.example",
		"SKILL_STORE_URL":               "http://skills.example",
		"TOOL_STORE_URL":                "http://tools.example",
		"KANBAN_STORE_URL":              "http://kanban.example",
		"KANBAN_STORE_SERVICE_TOKEN":    "kanban-token",
		"LLMBRIDGE_SERVICE_TOKEN":       "bridge-token",
		"REFERENCE_STORE_URL":           "http://references.example",
		"REFERENCE_STORE_SERVICE_TOKEN": "reference-token",
	}
	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(operatorValues))
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range SettingDefinitions() {
		want, named := operatorValues[definition.EnvironmentVariable]
		if !named {
			t.Errorf("%s is declared and this test sets no value for it", definition.EnvironmentVariable)
		}
		if got := set.String(definition.Key); got != want {
			t.Errorf("%s = %q, want the value of %s", definition.Key, got, definition.EnvironmentVariable)
		}
	}
	if len(operatorValues) != len(SettingDefinitions()) {
		t.Errorf("this test sets %d variables and %d are declared", len(operatorValues), len(SettingDefinitions()))
	}

	// A variable set to the empty string is the same as unset, as it was when
	// environmentOr compared os.Getenv to "".
	empty, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"GRANT_STORE_ADDR": "", "KANBAN_STORE_URL": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if empty.String(SettingListenAddress) != "127.0.0.1:8315" || empty.String(SettingKanbanStoreURL) != "http://127.0.0.1:8305" {
		t.Errorf("empty variables: listen=%q kanban=%q", empty.String(SettingListenAddress), empty.String(SettingKanbanStoreURL))
	}
}

func TestTheRegistryRefusesAGrantStoreVariableNobodyDeclared(t *testing.T) {
	_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"GRANT_STORE_ADDRESS": "127.0.0.1:8315"}))
	if err == nil || !strings.Contains(err.Error(), "GRANT_STORE_ADDRESS is set and grant-store declares no such setting") {
		t.Fatalf("NewSettingsRegistry = %v, want a refusal naming the misspelled variable", err)
	}
	// The owners' variables share their prefixes with other services' variables,
	// so a neighbor of one is not this service's to refuse.
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"GRANT_STORE_ADDR": ":1", "KANBAN_STORE_ADDR": ":8305", "PATH": "/bin"})); err != nil {
		t.Errorf("a declared variable and two outside the prefix were refused: %v", err)
	}
}

func TestAMissingServiceTokenIsNamedByCheckRequired(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatalf("a registry with no token must still build, for a test and for GET /settings' own tests: %v", err)
	}
	if err := registry.CheckRequired(); err == nil || !strings.Contains(err.Error(), "GRANT_STORE_SERVICE_TOKEN is unset") {
		t.Fatalf("CheckRequired = %v, want it to name the unset token", err)
	}
}

func newSettingsTestServer(t *testing.T, environment map[string]string) *httptest.Server {
	t.Helper()
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(environment))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterSettingsHandler(mux, registry, newFakeDirectory(), PrincipalEnforcement{ServiceToken: enforcementTestServiceToken})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const kanbanTokenThatMustNotBeServed = "kanban-store-token-that-must-not-be-served"

func TestGetSettingsAnswersOnlyAnUnrestrictedCaller(t *testing.T) {
	srv := newSettingsTestServer(t, map[string]string{
		"GRANT_STORE_ADDR":           "127.0.0.1:9999",
		"GRANT_STORE_SERVICE_TOKEN":  enforcementTestServiceToken,
		"KANBAN_STORE_SERVICE_TOKEN": kanbanTokenThatMustNotBeServed,
	})

	if status, body := doWithHeaders(t, srv, nil, http.MethodGet, "/settings", nil); status != http.StatusUnauthorized {
		t.Errorf("GET /settings with no caller = %d: %s", status, body)
	}
	if status, body := doWithHeaders(t, srv, map[string]string{ServiceTokenHeader: "not-the-token-0123456789-0123456789"}, http.MethodGet, "/settings", nil); status != http.StatusUnauthorized {
		t.Errorf("GET /settings with a wrong service token = %d: %s", status, body)
	}
	if status, body := doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), http.MethodGet, "/settings", nil); status != http.StatusForbidden {
		t.Errorf("GET /settings as a plain member = %d: %s", status, body)
	}
	if status, body := doWithHeaders(t, srv, asGrantStorePrincipal("principal_000008"), http.MethodGet, "/settings", nil); status != http.StatusOK {
		t.Errorf("GET /settings as an administrator = %d: %s", status, body)
	}

	status, body := doWithHeaders(t, srv, asGrantStoreService, http.MethodGet, "/settings", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /settings with the service token = %d: %s", status, body)
	}
	if strings.Contains(string(body), enforcementTestServiceToken) || strings.Contains(string(body), kanbanTokenThatMustNotBeServed) {
		t.Fatalf("GET /settings served a token's value")
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(body, &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != ServiceName || len(described.Settings) != len(SettingDefinitions()) {
		t.Fatalf("service=%q with %d settings, want %q with %d", described.Service, len(described.Settings), ServiceName, len(SettingDefinitions()))
	}
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and no write route is mounted", setting.Key)
		}
		switch setting.Key {
		case SettingListenAddress:
			if setting.Value != "127.0.0.1:9999" || setting.Source != msg.ServiceSettingSourceEnvironment {
				t.Errorf("listen address served as %q from %q", setting.Value, setting.Source)
			}
		case SettingServiceToken, SettingKanbanStoreServiceToken:
			if setting.Kind != msg.ServiceSettingKindSecret || !setting.IsSet || setting.Value != "" {
				t.Errorf("%s served as kind=%q is_set=%t value=%q, want a set secret with no value", setting.Key, setting.Kind, setting.IsSet, setting.Value)
			}
		}
	}

	if status, _ := doWithHeaders(t, srv, asGrantStoreService, http.MethodPut, "/settings/"+SettingListenAddress, map[string]string{"value": ":1"}); status == http.StatusOK {
		t.Errorf("PUT /settings/%s = 200: a write route is mounted", SettingListenAddress)
	}
}

func TestRegisterSettingsHandlerRefusesAShortTokenAndAMissingDirectory(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	for name, register := range map[string]func(){
		"a short token": func() {
			RegisterSettingsHandler(http.NewServeMux(), registry, newFakeDirectory(), PrincipalEnforcement{ServiceToken: "short"})
		},
		"a missing directory": func() {
			RegisterSettingsHandler(http.NewServeMux(), registry, nil, PrincipalEnforcement{ServiceToken: enforcementTestServiceToken})
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RegisterSettingsHandler with %s did not panic: GET /settings would be mounted with no working gate", name)
				}
			}()
			register()
		}()
	}
}

// Every environment variable the service's own code reads by name is declared.
// A read that is not declared is invisible on the settings page and escapes the
// startup check.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range SettingDefinitions() {
		declared[definition.EnvironmentVariable] = true
	}
	// Read by name and not settings of this service.
	notSettings := map[string]bool{}

	filesRead := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		filesRead++
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall || len(call.Args) == 0 {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			packageName, isIdentifier := selector.X.(*ast.Ident)
			if !isIdentifier || packageName.Name != "os" || (selector.Sel.Name != "Getenv" && selector.Sel.Name != "LookupEnv") {
				return true
			}
			literal, isLiteral := call.Args[0].(*ast.BasicLit)
			if !isLiteral {
				t.Errorf("%s reads an environment variable whose name is computed, which no declaration can be held to", path)
				return true
			}
			name, _ := strconv.Unquote(literal.Value)
			if !declared[name] && !notSettings[name] {
				t.Errorf("%s reads %s, which SettingDefinitions does not declare", path, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The walk starts at the package directory, which is the repository root. If
	// the package moves, the walk would read nothing and pass.
	if _, err := os.Stat(filepath.Join("cmd", "grant-store", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 3 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
