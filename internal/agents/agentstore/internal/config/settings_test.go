package config

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
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// repositoryRoot is where the source scan starts: this package is
// internal/config.
const repositoryRoot = "../.."

// definitionsByCommandDirectory is the declaration list each directory under
// cmd/ is held to. Every other directory is library code, which llm-bridge-server
// and inber embed, and is held to none.
var definitionsByCommandDirectory = map[string][]servicesettings.Definition{
	"cmd/server":        ServerSettingDefinitions(),
	"cmd/seed":          SeedCommandSettingDefinitions(),
	"cmd/migrate-inber": MigrateInberCommandSettingDefinitions(),
	"cmd/test-cycle":    nil,
}

// The registries give each command what its os.Getenv reads gave it before
// 2026-09-24: the same defaults with nothing set, and the operator's values
// when they are.
func TestTheRegistriesReadTheSameValuesTheCommandsAlwaysDid(t *testing.T) {
	nothing := servicesettings.MapEnvironment(map[string]string{})
	server, err := NewServerSettingsRegistry(nothing)
	if err != nil {
		t.Fatal(err)
	}
	// The one deliberate change: before 2026-09-24 the default was ":8300",
	// every interface, for a server with no auth on its prompt write routes.
	if got := server.String(SettingListenAddress); got != "127.0.0.1:8300" {
		t.Errorf("listen address with nothing set = %q, want loopback only", got)
	}
	if got := server.String(SettingDatabasePath); got != DefaultDatabasePath() || !strings.HasSuffix(got, filepath.Join(".config", "agent-store", "agents.db")) {
		t.Errorf("database with nothing set = %q", got)
	}
	if got := server.Integer(SettingAutoScanIntervalInSeconds); got != 900 {
		t.Errorf("scan interval with nothing set = %d, want 900 (15 minutes)", got)
	}
	seed, err := NewSeedCommandSettingsRegistry(nothing)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if got := seed.String(SettingInberRepositoryPath); got != filepath.Join(home, "repos", "inber") {
		t.Errorf("seed's inber root with nothing set = %q", got)
	}
	if got := seed.String(SettingDatabasePath); got != DefaultDatabasePath() {
		t.Errorf("seed's database with nothing set = %q", got)
	}
	migrate, err := NewMigrateInberCommandSettingsRegistry(nothing)
	if err != nil {
		t.Fatal(err)
	}
	if got := migrate.String(SettingInberRepositoryPath); got != filepath.Join(home, "repos", "inber") {
		t.Errorf("migrate-inber's inber path with nothing set = %q", got)
	}

	set := servicesettings.MapEnvironment(map[string]string{
		"AGENT_STORE_ADDR":               "127.0.0.1:8300",
		"AGENT_STORE_DB":                 "/srv/agents.db",
		"AGENT_STORE_SCAN_INTERVAL_SECS": "0",
		"INBER_ROOT":                     "/srv/inber-root",
		"INBER_PATH":                     "/srv/inber-path",
	})
	server, err = NewServerSettingsRegistry(set)
	if err != nil {
		t.Fatal(err)
	}
	if server.String(SettingListenAddress) != "127.0.0.1:8300" || server.String(SettingDatabasePath) != "/srv/agents.db" || server.Integer(SettingAutoScanIntervalInSeconds) != 0 {
		t.Errorf("server values not read back: %+v", server.Describe())
	}
	seed, err = NewSeedCommandSettingsRegistry(set)
	if err != nil {
		t.Fatal(err)
	}
	if seed.String(SettingDatabasePath) != "/srv/agents.db" || seed.String(SettingInberRepositoryPath) != "/srv/inber-root" {
		t.Errorf("seed values not read back: %+v", seed.Describe())
	}
	migrate, err = NewMigrateInberCommandSettingsRegistry(set)
	if err != nil {
		t.Fatal(err)
	}
	if migrate.String(SettingInberRepositoryPath) != "/srv/inber-path" {
		t.Errorf("migrate-inber value not read back: %+v", migrate.Describe())
	}

	// A variable set to the empty string is the same as unset, as it was when
	// the commands compared os.Getenv to "".
	empty, err := NewServerSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"AGENT_STORE_ADDR": "", "AGENT_STORE_SCAN_INTERVAL_SECS": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if empty.String(SettingListenAddress) != "127.0.0.1:8300" || empty.Integer(SettingAutoScanIntervalInSeconds) != 900 {
		t.Errorf("empty variables: address=%q interval=%d", empty.String(SettingListenAddress), empty.Integer(SettingAutoScanIntervalInSeconds))
	}
}

func TestTheServerRefusesAMisspelledVariableAnIntervalThatIsNotANumberAndNotAnotherServicesVariable(t *testing.T) {
	for _, misspelled := range []string{"AGENT_STORE_DB_PATH", "AGENT_STORE_ADDRESS"} {
		_, err := NewServerSettingsRegistry(servicesettings.MapEnvironment(map[string]string{misspelled: "x"}))
		if err == nil || !strings.Contains(err.Error(), misspelled+" is set and agent-store declares no such setting") {
			t.Errorf("%s: NewServerSettingsRegistry = %v, want a refusal naming it", misspelled, err)
		}
	}
	// This used to be logged and replaced with 15 minutes.
	_, err := NewServerSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"AGENT_STORE_SCAN_INTERVAL_SECS": "fifteen"}))
	if err == nil || !strings.Contains(err.Error(), "AGENT_STORE_SCAN_INTERVAL_SECS") {
		t.Errorf("NewServerSettingsRegistry = %v, want a refusal naming the interval", err)
	}
	notOurs := map[string]string{
		"AGENT_STORE_URL":              "http://localhost:8300",
		"AGENT_STORE_PATH":             "/x.db",
		"AGENT_STORE_LIVE_PROMPT_HOME": "/x",
		"PATH":                         "/bin",
		"HOME":                         "/root",
	}
	if _, err := NewServerSettingsRegistry(servicesettings.MapEnvironment(notOurs)); err != nil {
		t.Errorf("variables meant for others were refused: %v", err)
	}
	// The operator commands own nothing, so a server variable in the
	// operator's shell does not stop them.
	serverShell := servicesettings.MapEnvironment(map[string]string{"AGENT_STORE_ADDR": ":8300", "AGENT_STORE_SCAN_INTERVAL_SECS": "60"})
	if _, err := NewSeedCommandSettingsRegistry(serverShell); err != nil {
		t.Errorf("seed refused the server's variables: %v", err)
	}
	if _, err := NewMigrateInberCommandSettingsRegistry(serverShell); err != nil {
		t.Errorf("migrate-inber refused the server's variables: %v", err)
	}
}

// Every owned prefix is the start of a declared server variable. An owned
// prefix that starts nothing guards nothing and reads as if it did.
func TestEveryOwnedPrefixStartsADeclaredVariable(t *testing.T) {
	for _, prefix := range OwnedEnvironmentVariablePrefixes {
		found := false
		for _, definition := range ServerSettingDefinitions() {
			found = found || strings.HasPrefix(definition.EnvironmentVariable, prefix)
		}
		if !found {
			t.Errorf("owned prefix %s starts no declared variable", prefix)
		}
	}
}

func TestGetSettingsDescribesTheServerAndNothingCanBeWritten(t *testing.T) {
	registry, err := NewServerSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"AGENT_STORE_ADDR": "127.0.0.1:9300"}))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /settings", servicesettings.Handler(registry, "/settings"))

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d: %s", recorder.Code, recorder.Body)
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != ServerName || len(described.Settings) != len(ServerSettingDefinitions()) {
		t.Fatalf("service=%q with %d settings, want %q with %d", described.Service, len(described.Settings), ServerName, len(ServerSettingDefinitions()))
	}
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and the standalone server has no operator gate to put a write behind", setting.Key)
		}
		if setting.Key == SettingListenAddress && (setting.Value != "127.0.0.1:9300" || setting.Source != msg.ServiceSettingSourceEnvironment) {
			t.Errorf("listen address served as %q from %q", setting.Value, setting.Source)
		}
	}

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/settings/"+SettingListenAddress, strings.NewReader(`{"value":"1"}`)))
	if recorder.Code == http.StatusOK {
		t.Errorf("PUT /settings/%s = 200: a write route is mounted", SettingListenAddress)
	}
}

// Every environment variable a command reads by name is declared in that
// command's own list, and the library reads none. A read that is not declared
// is invisible on the settings page and escapes the startup check. The walk is
// scheduler's environmentReadFaults, as marginalia copied it, held per
// directory as multichat holds its binaries.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	commandDirectoriesSeen := map[string]bool{}
	filesRead := 0
	err := filepath.WalkDir(repositoryRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == ".git") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(repositoryRoot, path)
		if err != nil {
			return err
		}
		directory := filepath.ToSlash(filepath.Dir(relative))
		declared := map[string]bool{}
		if strings.HasPrefix(directory, "cmd/") {
			definitions, listed := definitionsByCommandDirectory[directory]
			if !listed {
				t.Errorf("%s is a command with no declaration list in definitionsByCommandDirectory", directory)
			}
			commandDirectoriesSeen[directory] = true
			for _, definition := range definitions {
				declared[definition.EnvironmentVariable] = true
			}
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		filesRead++
		for _, fault := range environmentReadFaults(file, declared) {
			t.Errorf("%s %s", relative, fault)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// If this package moved, the walk would start somewhere else, read nothing
	// and pass.
	if _, err := os.Stat(filepath.Join(repositoryRoot, "cmd", "server", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 15 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
	var listedButAbsent []string
	for directory := range definitionsByCommandDirectory {
		if !commandDirectoriesSeen[directory] {
			listedButAbsent = append(listedButAbsent, directory)
		}
	}
	sort.Strings(listedButAbsent)
	if len(listedButAbsent) > 0 {
		t.Errorf("declaration lists for commands that do not exist: %v", listedButAbsent)
	}
}

// The scan's own controls: each shape it exists to refuse is refused.
func TestTheSourceScanRefusesEachShapeOfUndeclaredRead(t *testing.T) {
	declared := map[string]bool{"AGENT_STORE_DB": true}
	for name, source := range map[string]string{
		"an undeclared name":      `package p; import "os"; var v = os.Getenv("AGENT_STORE_DBS")`,
		"a computed name":         `package p; import "os"; var n = "X"; var v = os.Getenv(n)`,
		"the whole environment":   `package p; import "os"; var v = os.Environ()`,
		"os.Getenv as a value":    `package p; import "os"; var read = os.Getenv`,
		"an undeclared LookupEnv": `package p; import "os"; func f() { os.LookupEnv("OTHER") }`,
		"os.ExpandEnv":            `package p; import "os"; var v = os.ExpandEnv("$HOME/repos")`,
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "control.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		if faults := environmentReadFaults(file, declared); len(faults) == 0 {
			t.Errorf("%s: the scan found nothing", name)
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), "control.go", `package p; import "os"; var v = os.Getenv("AGENT_STORE_DB")`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if faults := environmentReadFaults(file, declared); len(faults) != 0 {
		t.Errorf("a declared read was refused: %v", faults)
	}
}

// environmentReadFaults is marginalia's, with os.ExpandEnv added:
// cmd/migrate-inber read $HOME through it, and it reads any name its argument
// spells.
func environmentReadFaults(file *ast.File, declared map[string]bool) []string {
	var faults []string
	called := map[*ast.SelectorExpr]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector || !isOsFunction(selector, "Getenv", "LookupEnv") {
			return true
		}
		called[selector] = true
		literal, isLiteral := call.Args[0].(*ast.BasicLit)
		if !isLiteral {
			faults = append(faults, "reads an environment variable whose name is computed, which no declaration can be held to")
			return true
		}
		name, _ := strconv.Unquote(literal.Value)
		if !declared[name] {
			faults = append(faults, "reads "+name+", which its declaration list does not declare")
		}
		return true
	})
	ast.Inspect(file, func(node ast.Node) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		if isOsFunction(selector, "Environ", "ExpandEnv") {
			faults = append(faults, "reads the environment through os."+selector.Sel.Name+", which no declaration can be held to")
		}
		if isOsFunction(selector, "Getenv", "LookupEnv") && !called[selector] {
			faults = append(faults, "hands os."+selector.Sel.Name+" on as a value, so the names it reads cannot be seen here")
		}
		return true
	})
	return faults
}

func isOsFunction(selector *ast.SelectorExpr, names ...string) bool {
	packageName, isIdentifier := selector.X.(*ast.Ident)
	if !isIdentifier || packageName.Name != "os" {
		return false
	}
	for _, name := range names {
		if selector.Sel.Name == name {
			return true
		}
	}
	return false
}
