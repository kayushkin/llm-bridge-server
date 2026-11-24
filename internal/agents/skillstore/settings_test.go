package skillstore

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

// The registry gives the two commands what their four os.Getenv reads gave them
// before 2026-09-20 (":8301" then, loopback since 2026-11-05), the default data directory and the default install
// directory with nothing set, and the operator's values when they are set.
func TestTheRegistryReadsTheSameValuesTheCommandsAlwaysDid(t *testing.T) {
	unset, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"HOME": "/home/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := unset.String(SettingListenAddress); got != "127.0.0.1:8301" {
		t.Errorf("listen address with nothing set = %q, want 127.0.0.1:8301", got)
	}
	if got := unset.String(SettingDataDirectory); got != DefaultDataDir() {
		t.Errorf("data directory with nothing set = %q, want what Open(\"\") uses, %q", got, DefaultDataDir())
	}
	if got := unset.String(SettingInstallDirectory); got != DefaultInstallDir() {
		t.Errorf("install directory with nothing set = %q, want what Open leaves on the store, %q", got, DefaultInstallDir())
	}

	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"SKILL_STORE_ADDR":        "127.0.0.1:9999",
		"SKILL_STORE_DATA_DIR":    "/srv/skills/data",
		"SKILL_STORE_INSTALL_DIR": "/srv/skills/installed",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := set.String(SettingListenAddress); got != "127.0.0.1:9999" {
		t.Errorf("listen address = %q", got)
	}
	if got := set.String(SettingDataDirectory); got != "/srv/skills/data" {
		t.Errorf("data directory = %q", got)
	}
	if got := set.String(SettingInstallDirectory); got != "/srv/skills/installed" {
		t.Errorf("install directory = %q", got)
	}

	// A variable set to the empty string is the same as unset, as it was when
	// the commands compared os.Getenv to "". main only called SetInstallDir for
	// a non-empty SKILL_STORE_INSTALL_DIR, so an empty one has to land back on
	// DefaultInstallDir here too.
	empty, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"SKILL_STORE_ADDR":        "",
		"SKILL_STORE_DATA_DIR":    "",
		"SKILL_STORE_INSTALL_DIR": "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if empty.String(SettingListenAddress) != "127.0.0.1:8301" {
		t.Errorf("empty listen address = %q", empty.String(SettingListenAddress))
	}
	if empty.String(SettingDataDirectory) != DefaultDataDir() {
		t.Errorf("empty data directory = %q", empty.String(SettingDataDirectory))
	}
	if empty.String(SettingInstallDirectory) != DefaultInstallDir() {
		t.Errorf("empty install directory = %q", empty.String(SettingInstallDirectory))
	}
}

// The declared defaults are the directories the store itself resolves, not a
// second copy of them that could drift. Open("") picks the data directory and
// Open leaves DefaultInstallDir() on the store; the page must show those.
func TestTheDeclaredDefaultDirectoriesAreTheOnesTheStoreResolves(t *testing.T) {
	// A throwaway home, so opening the store cannot touch the live
	// ~/.config/skill-store or ~/.claude/skills.
	t.Setenv("HOME", t.TempDir())

	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := registry.String(SettingDataDirectory), store.DataDir(); got != want {
		t.Errorf("the declared data directory default is %q; Open(\"\") uses %q", got, want)
	}
	if got, want := registry.String(SettingInstallDirectory), store.InstallDir(); got != want {
		t.Errorf("the declared install directory default is %q; the store uses %q", got, want)
	}
}

func TestTheRegistryRefusesASkillStoreVariableNobodyDeclared(t *testing.T) {
	_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SKILL_STORE_ADDRESS": ":8301"}))
	if err == nil || !strings.Contains(err.Error(), "SKILL_STORE_ADDRESS is set and skill-store declares no such setting") {
		t.Fatalf("NewSettingsRegistry = %v, want a refusal naming the misspelled variable", err)
	}
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SKILL_STORE_DATA_DIRECTORY": "/tmp/x"})); err == nil {
		t.Error("SKILL_STORE_DATA_DIRECTORY was accepted; it is under an owned prefix and nobody declares it")
	}
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SKILL_STORE_INSTALL_DIRS": "/tmp/x"})); err == nil {
		t.Error("SKILL_STORE_INSTALL_DIRS was accepted; it is under an owned prefix and nobody declares it")
	}
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SKILL_STORE_ADDR": ":1", "PATH": "/bin", "HOME": "/root"})); err != nil {
		t.Errorf("a declared variable and two outside the prefixes were refused: %v", err)
	}
	// SKILL_STORE_URL is how the rest of the fleet finds this service, and it
	// reaches cmd/seed through the shell that runs it. Refusing it would stop
	// the seed command, which is why the owned prefixes are the full names.
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SKILL_STORE_URL": "http://127.0.0.1:8301"})); err != nil {
		t.Errorf("SKILL_STORE_URL was refused, which would stop cmd/seed in any shell that has it: %v", err)
	}
}

func TestGetSettingsDescribesTheServiceAndNothingCanBeWritten(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SKILL_STORE_ADDR": ":9999"}))
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
	if _, err := os.Stat(filepath.Join("cmd", "skill-store", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join("cmd", "seed", "main.go")); err != nil {
		t.Fatalf("the scan cannot see the seed command, which reads the environment too: %v", err)
	}
	if filesRead < 3 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
