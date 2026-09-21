package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The registry gives the service what envOr and os.Getenv gave it before
// 2026-09-21, with nothing set and with every variable set.
func TestLoadReadsTheSameValuesItAlwaysDid(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	unset, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Port:         8092,
		DatabasePath: "/home/someone/.scheduler/scheduler.db",
		Token:        "",
		NATSURL:      "nats://localhost:4222",
		BridgeURL:    "http://localhost:8160",
		NoteboardURL: "http://localhost:8191",
	}
	if got := Load(unset); got != want {
		t.Errorf("Load with nothing set = %+v, want %+v", got, want)
	}
	if err := unset.CheckRequired(); err != nil {
		t.Errorf("CheckRequired with a known home directory = %v", err)
	}

	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(everyVariableSetToItsOwnValue))
	if err != nil {
		t.Fatal(err)
	}
	want = Config{
		Port:         19104,
		DatabasePath: "/srv/scheduler.db",
		Token:        "a-token",
		NATSURL:      "nats://127.0.0.1:14222",
		BridgeURL:    "http://bridge.test:1",
		NoteboardURL: "http://noteboard.test:2",
	}
	if got := Load(set); got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}

	// A variable set to the empty string is the same as unset, as it was when
	// envOr compared os.Getenv to "".
	empty, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SCHEDULER_PORT": "", "NATS_URL": "", "LLMBRIDGE_URL": "", "NOTEBOARD_URL": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if got := Load(empty); got.Port != 8092 || got.NATSURL != DefaultNATSURL || got.BridgeURL != DefaultBridgeURL || got.NoteboardURL != DefaultNoteboardURL {
		t.Errorf("empty variables: %+v", got)
	}
}

var everyVariableSetToItsOwnValue = map[string]string{
	"SCHEDULER_PORT":  "19104",
	"SCHEDULER_DB":    "/srv/scheduler.db",
	"SCHEDULER_TOKEN": "a-token",
	"NATS_URL":        "nats://127.0.0.1:14222",
	"LLMBRIDGE_URL":   "http://bridge.test:1",
	"NOTEBOARD_URL":   "http://noteboard.test:2",
}

// The same-values test sets every declared variable. A definition added
// without a row there is a setting nothing reads back.
func TestTheSameValuesTestCoversEveryDeclaredSetting(t *testing.T) {
	for _, definition := range SettingDefinitions() {
		if _, covered := everyVariableSetToItsOwnValue[definition.EnvironmentVariable]; !covered {
			t.Errorf("%s is declared and TestLoadReadsTheSameValuesItAlwaysDid does not set it", definition.EnvironmentVariable)
		}
	}
}

// Before 2026-09-21 an empty HOME opened .scheduler/scheduler.db in the working
// directory, silently: a scheduler with no jobs. Now the setting is unset and
// the service refuses to start.
func TestAnUnknownHomeDirectoryLeavesTheDatabaseUnsetAndCheckRequiredSaysSo(t *testing.T) {
	t.Setenv("HOME", "")
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err == nil || !strings.Contains(err.Error(), "SCHEDULER_DB is unset") {
		t.Fatalf("CheckRequired = %v, want it to name SCHEDULER_DB", err)
	}
}

// SCHEDULER_URL is how jobs find the scheduler, and jobs are its children; a
// misspelling of an owned name is refused.
func TestTheRegistryRefusesAMisspellingAndNotTheVariableJobsUseToFindIt(t *testing.T) {
	for _, misspelled := range []string{"SCHEDULER_DB_PATH", "SCHEDULER_PORTS", "SCHEDULER_TOKEN_FILE"} {
		_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{misspelled: "x"}))
		if err == nil || !strings.Contains(err.Error(), misspelled+" is set and scheduler declares no such setting") {
			t.Errorf("NewSettingsRegistry with %s = %v, want a refusal naming it", misspelled, err)
		}
	}
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SCHEDULER_URL": "http://localhost:8092", "SCHEDULER_SOURCE": "x", "LLM_BRIDGE_URL": "http://localhost:8160"})); err != nil {
		t.Errorf("a variable the service does not own was refused: %v", err)
	}
	// A port that is not a whole number stops the start at the registry rather
	// than at ListenAndServe.
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SCHEDULER_PORT": ":8092"})); err == nil {
		t.Error("SCHEDULER_PORT=:8092 was accepted")
	}
}

// The service's packages: cmd/scheduler and every package of this module it
// imports, found by following imports rather than listed by hand, so a new
// import is scanned the day it is added.
func servicePackageDirectories(t *testing.T, repositoryRoot string) []string {
	t.Helper()
	const module = "github.com/slava/scheduler/"
	seen := map[string]bool{}
	queue := []string{"cmd/scheduler"}
	for len(queue) > 0 {
		directory := queue[0]
		queue = queue[1:]
		if seen[directory] {
			continue
		}
		seen[directory] = true
		for _, path := range nonTestGoFiles(t, filepath.Join(repositoryRoot, directory)) {
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range file.Imports {
				importPath, _ := strconv.Unquote(imported.Path.Value)
				if strings.HasPrefix(importPath, module) {
					queue = append(queue, strings.TrimPrefix(importPath, module))
				}
			}
		}
	}
	directories := make([]string, 0, len(seen))
	for directory := range seen {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	return directories
}

func nonTestGoFiles(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			paths = append(paths, filepath.Join(directory, name))
		}
	}
	return paths
}

// Every environment variable the service's code reads by name is declared. A
// read that is not declared is invisible on the settings page and escapes the
// startup check.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range SettingDefinitions() {
		declared[definition.EnvironmentVariable] = true
	}
	const repositoryRoot = "../.."
	// The walk starts two directories above this package, which is the
	// repository root. If the package moves, the walk would read the wrong tree
	// and pass.
	if _, err := os.Stat(filepath.Join(repositoryRoot, "cmd", "scheduler", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}

	directories := servicePackageDirectories(t, repositoryRoot)
	// The packages the service is made of today. Fewer means the import walk
	// is broken and would pass by reading nothing.
	for _, want := range []string{"cmd/scheduler", "internal/api", "internal/bus", "internal/config", "internal/cron", "internal/db", "internal/executor", "internal/logging"} {
		if !contains(directories, want) {
			t.Errorf("the scan does not reach %s; it reached %v", want, directories)
		}
	}

	filesRead := 0
	for _, directory := range directories {
		for _, path := range nonTestGoFiles(t, filepath.Join(repositoryRoot, directory)) {
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			filesRead++
			for _, fault := range environmentReadFaults(file, declared) {
				t.Errorf("%s %s", path, fault)
			}
		}
	}
	if filesRead < 10 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}

// environmentReadFaults lists every read of the environment in file that the
// declarations cannot hold: a name nobody declared, a computed name, the whole
// environment, or os.Getenv handed on as a value for someone else to call.
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
			faults = append(faults, "reads "+name+", which SettingDefinitions does not declare")
		}
		return true
	})
	ast.Inspect(file, func(node ast.Node) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		if isOsFunction(selector, "Environ") {
			faults = append(faults, "reads the whole environment, which no declaration can be held to")
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
	return isIdentifier && packageName.Name == "os" && contains(names, selector.Sel.Name)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
