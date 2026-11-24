package toolstore

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
	"github.com/kayushkin/tool-store/tools"
)

// The registry gives the service what its os.Getenv reads gave it before
// 2026-09-20 (":8302" then, loopback since 2026-11-05), the default data directory, auth-store on 127.0.0.1:8303,
// PinchTab on localhost:9867 and the scheduler on localhost:8092 with nothing
// set, no secret, and the operator's values when they are set.
func TestTheRegistryReadsTheSameValuesTheServiceAlwaysDid(t *testing.T) {
	unset, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"HOME": "/home/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		SettingListenAddress:  "127.0.0.1:8302",
		SettingDataDirectory:  DefaultDataDir(),
		SettingAuthStoreURL:   "http://127.0.0.1:8303",
		SettingAuthStoreToken: "",
		SettingPinchtabURL:    "http://localhost:9867",
		SettingPinchtabToken:  "",
		SettingBraveAPIKey:    "",
		SettingSchedulerURL:   "http://localhost:8092",
		SettingSchedulerToken: "",
	} {
		if got := unset.String(key); got != want {
			t.Errorf("%s with nothing set = %q, want %q", key, got, want)
		}
	}

	variables := map[string]string{
		"TOOL_STORE_ADDR":     "127.0.0.1:9999",
		"TOOL_STORE_DATA_DIR": "/srv/tools",
		"AUTH_STORE_URL":      "http://127.0.0.1:1",
		"AUTH_STORE_TOKEN":    "auth-store-token-text",
		"PINCHTAB_URL":        "http://127.0.0.1:2",
		"PINCHTAB_TOKEN":      "pinchtab-token-text",
		"BRAVE_API_KEY":       "brave-key-text",
		"SCHEDULER_URL":       "http://127.0.0.1:3",
		"SCHEDULER_TOKEN":     "scheduler-token-text",
	}
	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(variables))
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range SettingDefinitions() {
		if got := set.String(definition.Key); got != variables[definition.EnvironmentVariable] {
			t.Errorf("%s = %q, want the value of %s", definition.Key, got, definition.EnvironmentVariable)
		}
	}
	if len(variables) != len(SettingDefinitions()) {
		t.Errorf("%d variables set against %d definitions: a definition has no case here", len(variables), len(SettingDefinitions()))
	}

	// A variable set to the empty string is the same as unset, as it was when
	// the code compared os.Getenv to "".
	empty, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"TOOL_STORE_ADDR": "", "TOOL_STORE_DATA_DIR": "", "PINCHTAB_URL": "", "SCHEDULER_URL": "", "AUTH_STORE_URL": ""}))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		SettingListenAddress: "127.0.0.1:8302",
		SettingDataDirectory: DefaultDataDir(),
		SettingAuthStoreURL:  "http://127.0.0.1:8303",
		SettingPinchtabURL:   "http://localhost:9867",
		SettingSchedulerURL:  "http://localhost:8092",
	} {
		if got := empty.String(key); got != want {
			t.Errorf("%s with an empty variable = %q, want the default %q", key, got, want)
		}
	}
}

// The default data directory the page shows is the one Open("") opens.
func TestTheDeclaredDataDirectoryDefaultIsTheOneOpenUses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.String(SettingDataDirectory); got != store.DataDir() {
		t.Errorf("declared default %q, and Open(\"\") opened %q", got, store.DataDir())
	}
}

func TestTheRegistryRefusesAMisspellingOfAnOwnedVariableAndNothingElse(t *testing.T) {
	for _, misspelled := range []string{"TOOL_STORE_ADDRESS", "TOOL_STORE_DATA_DIRECTORY"} {
		_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{misspelled: "x"}))
		if err == nil || !strings.Contains(err.Error(), misspelled+" is set and tool-store declares no such setting") {
			t.Errorf("NewSettingsRegistry with %s = %v, want a refusal naming it", misspelled, err)
		}
	}
	// TOOL_STORE_URL is how other services find this one, and an agent's shell
	// carries it. It must not stop the binary.
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"TOOL_STORE_ADDR": ":1", "TOOL_STORE_URL": "http://localhost:8302", "PATH": "/bin", "HOME": "/root"})); err != nil {
		t.Errorf("a declared variable and three that are not this service's were refused: %v", err)
	}
}

// A refusal names variables. It must never carry the text of a secret that
// happens to be set beside the misspelling.
func TestARefusalDoesNotQuoteASecret(t *testing.T) {
	_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"TOOL_STORE_ADDRESS": "misspelled-value-text",
		"AUTH_STORE_TOKEN":   "auth-store-token-text",
		"BRAVE_API_KEY":      "brave-key-text",
	}))
	if err == nil {
		t.Fatal("the misspelled variable was accepted")
	}
	for _, text := range []string{"auth-store-token-text", "brave-key-text", "misspelled-value-text"} {
		if strings.Contains(err.Error(), text) {
			t.Errorf("the refusal quotes a value: %v", err)
		}
	}
}

func TestGetSettingsDescribesTheServiceHidesSecretsAndNothingCanBeWritten(t *testing.T) {
	secrets := map[string]string{
		"AUTH_STORE_TOKEN": "auth-store-token-text",
		"PINCHTAB_TOKEN":   "pinchtab-token-text",
		"BRAVE_API_KEY":    "brave-key-text",
		"SCHEDULER_TOKEN":  "scheduler-token-text",
	}
	variables := map[string]string{"TOOL_STORE_ADDR": ":9999"}
	for name, value := range secrets {
		variables[name] = value
	}
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(variables))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterSettingsHandler(mux, registry)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d: %s", recorder.Code, recorder.Body)
	}
	for name, value := range secrets {
		if strings.Contains(recorder.Body.String(), value) {
			t.Errorf("GET /settings carries the text of %s", name)
		}
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != ServiceName || len(described.Settings) != len(SettingDefinitions()) {
		t.Fatalf("service=%q with %d settings, want %q with %d", described.Service, len(described.Settings), ServiceName, len(SettingDefinitions()))
	}
	secretSettings := 0
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and this service has no operator gate to put a write behind", setting.Key)
		}
		if setting.Kind == msg.ServiceSettingKindSecret {
			secretSettings++
		}
		if setting.Key == SettingListenAddress && (setting.Value != ":9999" || setting.Source != msg.ServiceSettingSourceEnvironment) {
			t.Errorf("listen address served as %q from %q", setting.Value, setting.Source)
		}
	}
	if secretSettings != len(secrets) {
		t.Errorf("%d settings are declared secret, want %d", secretSettings, len(secrets))
	}

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/settings/"+SettingListenAddress, strings.NewReader(`{"value":":1"}`)))
	if recorder.Code == http.StatusOK {
		t.Errorf("PUT /settings/%s = 200: a write route is mounted", SettingListenAddress)
	}
	if got := registry.String(SettingListenAddress); got != ":9999" {
		t.Errorf("the refused write changed the listen address to %q", got)
	}
}

// Each of the five values the tools are built with comes from its own setting.
// Five distinct values, so two swapped fields cannot pass.
func TestTheToolsAreHandedTheirOwnSettings(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"PINCHTAB_URL":    "http://pinchtab.invalid",
		"PINCHTAB_TOKEN":  "pinchtab-token-text",
		"BRAVE_API_KEY":   "brave-key-text",
		"SCHEDULER_URL":   "http://scheduler.invalid",
		"SCHEDULER_TOKEN": "scheduler-token-text",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := tools.OutsideServiceConnections{
		Pinchtab:    tools.PinchtabConnection{BaseURL: "http://pinchtab.invalid", Token: "pinchtab-token-text"},
		BraveAPIKey: "brave-key-text",
		Scheduler:   tools.SchedulerConnection{BaseURL: "http://scheduler.invalid", Token: "scheduler-token-text"},
	}
	if got := OutsideServiceConnectionsFrom(registry); got != want {
		t.Errorf("the tools were handed %+v, want each value from its own setting", got)
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
	// Read by name and not settings of this service: tools/task_plan.go hands
	// the build command the PATH it already has.
	notSettings := map[string]bool{"PATH": true}
	// Files allowed a read whose name is computed, and why it is not a setting.
	// cli_runner.go looks up the env_keys a CLI tool's own row declares, to hand
	// them to the child it starts: the names are data in the tools table.
	computedNameReadsThatAreNotSettings := map[string]bool{"cli_runner.go": true}
	computedNameReadsSeen := map[string]bool{}

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
			if !isLiteral && computedNameReadsThatAreNotSettings[path] {
				computedNameReadsSeen[path] = true
				return true
			}
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
	for path := range computedNameReadsThatAreNotSettings {
		if !computedNameReadsSeen[path] {
			t.Errorf("%s is allowed a computed variable name and no longer has one: remove the allowance", path)
		}
	}
	// The walk starts at the package directory, which is the repository root. If
	// the package moves, the walk would read nothing and pass.
	if _, err := os.Stat(filepath.Join("cmd", "tool-store", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 3 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
