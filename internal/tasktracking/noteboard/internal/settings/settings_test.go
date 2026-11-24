package settings

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The registry gives the command what its two os.Getenv reads gave it before
// 2026-09-20: port 8191 and ~/.noteboard/noteboard.db with nothing set, and the
// operator's values when they are.
func TestTheRegistryReadsTheSameValuesTheCommandAlwaysDid(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	unset, err := NewRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := unset.Integer(ListenPort); got != 8191 {
		t.Errorf("port with nothing set = %d, want 8191", got)
	}
	if got := unset.String(DatabasePath); got != "/home/someone/.noteboard/noteboard.db" {
		t.Errorf("database with nothing set = %q, want /home/someone/.noteboard/noteboard.db", got)
	}
	if err := unset.CheckRequired(); err != nil {
		t.Errorf("CheckRequired with a known home directory = %v", err)
	}

	set, err := NewRegistry(servicesettings.MapEnvironment(map[string]string{"NOTEBOARD_PORT": "9999", "NOTEBOARD_DB": "/srv/notes.db"}))
	if err != nil {
		t.Fatal(err)
	}
	if set.Integer(ListenPort) != 9999 || set.String(DatabasePath) != "/srv/notes.db" {
		t.Errorf("port=%d database=%q", set.Integer(ListenPort), set.String(DatabasePath))
	}

	// A variable set to the empty string is the same as unset, as it was when
	// the command compared os.Getenv to "".
	empty, err := NewRegistry(servicesettings.MapEnvironment(map[string]string{"NOTEBOARD_PORT": "", "NOTEBOARD_DB": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Integer(ListenPort) != 8191 || empty.String(DatabasePath) != "/home/someone/.noteboard/noteboard.db" {
		t.Errorf("empty variables: port=%d database=%q", empty.Integer(ListenPort), empty.String(DatabasePath))
	}
}

// Before 2026-09-20 an unknown home directory opened .noteboard/noteboard.db
// relative to wherever the process ran, silently. Now the setting is unset and
// the command refuses to start.
func TestAnUnknownHomeDirectoryLeavesTheDatabaseUnsetAndCheckRequiredSaysSo(t *testing.T) {
	t.Setenv("HOME", "")
	registry, err := NewRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err == nil || !strings.Contains(err.Error(), "NOTEBOARD_DB is unset") {
		t.Fatalf("CheckRequired = %v, want it to name NOTEBOARD_DB", err)
	}
}

func TestAPortThatIsNotAWholeNumberIsRefused(t *testing.T) {
	if _, err := NewRegistry(servicesettings.MapEnvironment(map[string]string{"NOTEBOARD_PORT": "eighty"})); err == nil || !strings.Contains(err.Error(), "NOTEBOARD_PORT") {
		t.Fatalf("NewRegistry = %v, want a refusal naming NOTEBOARD_PORT", err)
	}
}

// NOTEBOARD_URL is how other programs find noteboard. It shares the natural
// prefix and is not noteboard's, so it must never stop noteboard from starting;
// a misspelling of either declared name must.
func TestTheRegistryRefusesAMisspellingAndNotTheVariableOthersFindNoteboardBy(t *testing.T) {
	for _, misspelled := range []string{"NOTEBOARD_DB_PATH", "NOTEBOARD_PORTS"} {
		_, err := NewRegistry(servicesettings.MapEnvironment(map[string]string{misspelled: "x"}))
		if err == nil || !strings.Contains(err.Error(), misspelled+" is set and noteboard declares no such setting") {
			t.Errorf("NewRegistry with %s = %v, want a refusal naming it", misspelled, err)
		}
	}
	if _, err := NewRegistry(servicesettings.MapEnvironment(map[string]string{"NOTEBOARD_URL": "http://localhost:8191", "NOTEBOARD_PORT": "8191", "PATH": "/bin"})); err != nil {
		t.Errorf("NOTEBOARD_URL beside a declared variable was refused: %v", err)
	}
}

// Every environment variable the service's own code reads by name is declared.
// A read that is not declared is invisible on the settings page and escapes the
// startup check.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range Definitions() {
		declared[definition.EnvironmentVariable] = true
	}
	// Read by name and not settings of this service.
	notSettings := map[string]bool{}
	const repositoryRoot = "../.."

	filesRead := 0
	err := filepath.WalkDir(repositoryRoot, func(path string, entry fs.DirEntry, err error) error {
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
				t.Errorf("%s reads %s, which Definitions does not declare", path, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The walk starts two directories above this package, which is the
	// repository root. If the package moves, the walk would read the wrong tree
	// and pass.
	if _, err := os.Stat(filepath.Join(repositoryRoot, "cmd", "noteboard", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 3 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}

func TestTheServiceListensOnLoopbackWithNothingSet(t *testing.T) {
	registry, err := NewRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.String(ListenHost); got != "127.0.0.1" {
		t.Fatalf("listen host with nothing set is %q; every route is open, so it must be loopback", got)
	}
}
