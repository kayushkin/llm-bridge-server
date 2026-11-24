package bundlestore

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

// The registry gives the command what its four environment reads gave it before
// 2026-09-20 — port 8307 (bound to loopback since 2026-10-26), the default data
// directory, and the two store URLs with nothing set — and the operator's values
// when they are.
func TestTheRegistryReadsTheSameValuesTheCommandAlwaysDid(t *testing.T) {
	unset, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"HOME": "/srv/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := unset.String(SettingListenAddress); got != "127.0.0.1:8307" {
		t.Errorf("listen address with nothing set = %q, want 127.0.0.1:8307", got)
	}
	if got := unset.String(SettingDataDirectory); got != DefaultDataDir() {
		t.Errorf("data directory with nothing set = %q, want what Open(\"\") uses, %q", got, DefaultDataDir())
	}
	if got := unset.String(SettingSkillStoreURL); got != "http://localhost:8301" {
		t.Errorf("skill-store URL with nothing set = %q, want http://localhost:8301", got)
	}
	if got := unset.String(SettingToolStoreURL); got != "http://localhost:8302" {
		t.Errorf("tool-store URL with nothing set = %q, want http://localhost:8302", got)
	}

	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"BUNDLE_STORE_ADDR":     "127.0.0.1:9999",
		"BUNDLE_STORE_DATA_DIR": "/srv/bundles",
		"SKILL_STORE_URL":       "http://127.0.0.1:1",
		"TOOL_STORE_URL":        "http://127.0.0.1:2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := set.String(SettingListenAddress); got != "127.0.0.1:9999" {
		t.Errorf("listen address = %q", got)
	}
	if got := set.String(SettingDataDirectory); got != "/srv/bundles" {
		t.Errorf("data directory = %q", got)
	}
	if got := set.String(SettingSkillStoreURL); got != "http://127.0.0.1:1" {
		t.Errorf("skill-store URL = %q", got)
	}
	if got := set.String(SettingToolStoreURL); got != "http://127.0.0.1:2" {
		t.Errorf("tool-store URL = %q", got)
	}

	// A variable set to the empty string is the same as unset, as it was when
	// the command compared os.Getenv to "" and when envOr did.
	empty, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"BUNDLE_STORE_ADDR":     "",
		"BUNDLE_STORE_DATA_DIR": "",
		"SKILL_STORE_URL":       "",
		"TOOL_STORE_URL":        "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if empty.String(SettingListenAddress) != "127.0.0.1:8307" || empty.String(SettingDataDirectory) != DefaultDataDir() {
		t.Errorf("empty variables: listen=%q data=%q", empty.String(SettingListenAddress), empty.String(SettingDataDirectory))
	}
	if empty.String(SettingSkillStoreURL) != DefaultSkillStoreURL || empty.String(SettingToolStoreURL) != DefaultToolStoreURL {
		t.Errorf("empty variables: skill=%q tool=%q", empty.String(SettingSkillStoreURL), empty.String(SettingToolStoreURL))
	}
}

// The data directory is declared as DefaultDataDir(), so the settings page shows
// the directory in force rather than an empty value. Open("") resolves to the
// same directory, and this holds the two together. A throwaway home keeps it
// away from the live database.
func TestTheDeclaredDataDirectoryDefaultIsWhereOpenPutsTheDatabase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, definition := range SettingDefinitions() {
		if definition.Key != SettingDataDirectory {
			continue
		}
		if definition.Default != store.DataDir() {
			t.Fatalf("the data_directory default is %q and Open(\"\") uses %q", definition.Default, store.DataDir())
		}
		return
	}
	t.Fatalf("no %s setting is declared", SettingDataDirectory)
}

func TestTheRegistryRefusesABundleStoreVariableNobodyDeclared(t *testing.T) {
	_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"BUNDLE_STORE_ADDRESS": ":8307"}))
	if err == nil || !strings.Contains(err.Error(), "BUNDLE_STORE_ADDRESS is set and bundle-store declares no such setting") {
		t.Fatalf("NewSettingsRegistry = %v, want a refusal naming the misspelled variable", err)
	}
	// The two store URLs are declared, and their prefixes are not owned: a
	// SKILL_STORE_ or TOOL_STORE_ name this service does not read belongs to
	// somebody else and must pass.
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"BUNDLE_STORE_ADDR":      ":1",
		"SKILL_STORE_DATA_DIR":   "/srv/skills",
		"TOOL_STORE_SOMETHING":   "x",
		"LLMBRIDGE_BUNDLE_STORE": "x",
		"PATH":                   "/bin",
		"HOME":                   "/root",
	})); err != nil {
		t.Errorf("a declared variable and four outside the owned prefix were refused: %v", err)
	}
}

func TestGetSettingsDescribesTheServiceAndNothingCanBeWritten(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"BUNDLE_STORE_ADDR": ":9999"}))
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
		if setting.Key == SettingListenAddress && (setting.Value != ":9999" || setting.Source != msg.ServiceSettingSourceEnvironment) {
			t.Errorf("listen address served as %q from %q", setting.Value, setting.Source)
		}
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
	if _, err := os.Stat(filepath.Join("cmd", "bundle-store", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 3 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
