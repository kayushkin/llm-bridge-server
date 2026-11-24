package peoplestore

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

func TestTheRegistryReadsDefaultsAndTheOperatorsValues(t *testing.T) {
	unset, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"HOME": "/nonexistent"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := unset.String(SettingListenAddress); got != "127.0.0.1:8321" {
		t.Errorf("listen address with nothing set = %q, want 127.0.0.1:8321", got)
	}
	if got := unset.String(SettingDataDirectory); got != DefaultDataDir() {
		t.Errorf("data directory with nothing set = %q, want %q", got, DefaultDataDir())
	}
	if err := unset.CheckRequired(); err == nil || !strings.Contains(err.Error(), "PEOPLE_STORE_PRINCIPAL_STORE_URL is unset") {
		t.Errorf("CheckRequired with no principal-store address = %v, want a refusal naming it", err)
	}

	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"PEOPLE_STORE_ADDR":                "127.0.0.1:9999",
		"PEOPLE_STORE_DATA_DIR":            "/srv/people",
		"PEOPLE_STORE_PRINCIPAL_STORE_URL": "http://127.0.0.1:8314",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := set.CheckRequired(); err != nil {
		t.Fatal(err)
	}
	if set.String(SettingListenAddress) != "127.0.0.1:9999" || set.String(SettingDataDirectory) != "/srv/people" || set.String(SettingPrincipalStoreURL) != "http://127.0.0.1:8314" {
		t.Errorf("listen=%q data=%q principal-store=%q", set.String(SettingListenAddress), set.String(SettingDataDirectory), set.String(SettingPrincipalStoreURL))
	}
}

func TestTheRegistryRefusesAPeopleStoreVariableNobodyDeclared(t *testing.T) {
	_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"PEOPLE_STORE_ADDRESS": "127.0.0.1:8321"}))
	if err == nil || !strings.Contains(err.Error(), "PEOPLE_STORE_ADDRESS is set and people-store declares no such setting") {
		t.Fatalf("NewSettingsRegistry = %v, want a refusal naming the misspelled variable", err)
	}
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"PEOPLE_STORE_ADDR": ":1", "PATH": "/bin", "HOME": "/root"})); err != nil {
		t.Errorf("a declared variable and two outside the prefix were refused: %v", err)
	}
}

func TestTheImporterRegistryOwnsNoPrefixAndNeedsAllFour(t *testing.T) {
	registry, err := NewImporterSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SOMETHING_ELSE": "x", "PEOPLE_STORE_ADDR": "y"}))
	if err != nil {
		t.Fatalf("another service's variables were refused: %v", err)
	}
	err = registry.CheckRequired()
	for _, variable := range []string{"PEOPLE_STORE_URL", "MULTICHAT_URL", "AUTH_STORE_URL", "AUTH_STORE_TOKEN"} {
		if err == nil || !strings.Contains(err.Error(), variable+" is unset") {
			t.Errorf("CheckRequired = %v, want it to name %s", err, variable)
		}
	}
}

func TestGetSettingsDescribesTheServiceAndNothingCanBeWritten(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"PEOPLE_STORE_ADDR": ":9999"}))
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
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != ServiceName || len(described.Settings) != len(SettingDefinitions()) {
		t.Fatalf("service=%q with %d settings, want %q with %d", described.Service, len(described.Settings), ServiceName, len(SettingDefinitions()))
	}
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and this service has no operator gate to put a write behind", setting.Key)
		}
	}

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/settings/"+SettingListenAddress, strings.NewReader(`{"value":":1"}`)))
	if recorder.Code == http.StatusOK {
		t.Errorf("PUT /settings/%s = 200: a write route is mounted", SettingListenAddress)
	}
}

// Every environment variable the repo's code reads by name is declared, by the
// service or by the importer. A read that is not declared is invisible on the
// settings page and escapes the startup check.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range append(SettingDefinitions(), ImporterSettingDefinitions()...) {
		declared[definition.EnvironmentVariable] = true
	}

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
			if !declared[name] {
				t.Errorf("%s reads %s, which no settings definition declares", path, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join("cmd", "people-store", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 5 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
