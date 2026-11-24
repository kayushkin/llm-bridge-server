package journalstore

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

// No setting is required; every test's environment starts from nothing.
var withToken = map[string]string{}

func environment(extra map[string]string) servicesettings.Environment {
	merged := map[string]string{}
	for name, value := range withToken {
		merged[name] = value
	}
	for name, value := range extra {
		merged[name] = value
	}
	return servicesettings.MapEnvironment(merged)
}

func TestTheRegistryReadsDefaultsAndTheOperatorsValues(t *testing.T) {
	unset, err := NewSettingsRegistry(environment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := unset.CheckRequired(); err != nil {
		t.Errorf("with the token set, CheckRequired = %v", err)
	}
	if got := unset.String(SettingListenAddress); got != "127.0.0.1:8322" {
		t.Errorf("listen address with nothing set = %q, want 127.0.0.1:8322", got)
	}
	if got := unset.String(SettingDataDirectory); got != DefaultDataDir() {
		t.Errorf("data directory with nothing set = %q, want %q", got, DefaultDataDir())
	}

	set, err := NewSettingsRegistry(environment(map[string]string{
		"JOURNAL_STORE_ADDR":                "127.0.0.1:9999",
		"JOURNAL_STORE_DATA_DIR":            "/srv/journal",
		"JOURNAL_STORE_NOTEBOARD_URL":       "http://noteboard.test",
		"JOURNAL_STORE_REFERENCE_STORE_URL": "http://references.test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		SettingListenAddress: "127.0.0.1:9999", SettingDataDirectory: "/srv/journal",
		SettingNoteboardURL: "http://noteboard.test", SettingReferenceStoreURL: "http://references.test",
	} {
		if got := set.String(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestTheRegistryRefusesAJournalStoreVariableNobodyDeclared(t *testing.T) {
	_, err := NewSettingsRegistry(environment(map[string]string{"JOURNAL_STORE_ADDRESS": "127.0.0.1:8322"}))
	if err == nil || !strings.Contains(err.Error(), "JOURNAL_STORE_ADDRESS is set and journal-store declares no such setting") {
		t.Fatalf("NewSettingsRegistry = %v, want a refusal naming the misspelled variable", err)
	}
	if _, err := NewSettingsRegistry(environment(map[string]string{"JOURNAL_STORE_ADDR": ":1", "PATH": "/bin", "HOME": "/root"})); err != nil {
		t.Errorf("a declared variable and two outside the prefix were refused: %v", err)
	}
}

func TestGetSettingsDescribesTheServiceHidesTheTokenAndNothingCanBeWritten(t *testing.T) {
	registry, err := NewSettingsRegistry(environment(map[string]string{"JOURNAL_STORE_ADDR": ":9999"}))
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
	if strings.Contains(recorder.Body.String(), "test-token") {
		t.Errorf("GET /settings shows a secret's value")
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
	if _, err := os.Stat(filepath.Join("cmd", "journal-store", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 5 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
