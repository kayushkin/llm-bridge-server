package possessionstore

import (
	"flag"
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

func TestTheRegistryReadsDefaultsAndRefusesAnUndeclaredVariable(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.String(SettingListenAddress); got != "127.0.0.1:8341" {
		t.Errorf("listen address = %q, want loopback 127.0.0.1:8341", got)
	}
	if got := registry.ModelRole(SettingFirstReaderRole); got != "possession-photo-reader" {
		t.Errorf("first reader role = %q", got)
	}
	_, err = NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"POSSESSION_STORE_ADDRESS": ":1"}))
	if err == nil || !strings.Contains(err.Error(), "POSSESSION_STORE_ADDRESS") {
		t.Fatalf("an undeclared POSSESSION_STORE_ variable = %v, want a refusal naming it", err)
	}
	if _, err := NewAddPhotosSettingsRegistry(servicesettings.MapEnvironment(nil)); err != nil {
		t.Fatal(err)
	}
}

// Every environment variable the code reads by name is declared.
func TestEveryEnvironmentVariableTheCodeReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range append(SettingDefinitions(), AddPhotosSettingDefinitions()...) {
		declared[definition.EnvironmentVariable] = true
	}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector || (selector.Sel.Name != "Getenv" && selector.Sel.Name != "LookupEnv") || len(call.Args) != 1 {
				return true
			}
			literal, isLiteral := call.Args[0].(*ast.BasicLit)
			if !isLiteral {
				t.Errorf("%s reads an environment variable by a computed name", path)
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
}

// liveEnvironmentFile is a /proc/<pid>/environ to build the registry from;
// deploy.sh runs the test below against the running service before it stops
// it, because a set POSSESSION_STORE_ variable nobody declared stops the new
// binary at boot.
var liveEnvironmentFile = flag.String("live-environment-file", "", "a /proc/<pid>/environ to build the settings registry from")

// Only the verdict is printed, never a value: an environment holds secrets.
func TestTheLiveProcessEnvironmentBuildsARegistry(t *testing.T) {
	if *liveEnvironmentFile == "" {
		t.Skip("no -live-environment-file given")
	}
	content, err := os.ReadFile(*liveEnvironmentFile)
	if err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{}
	for _, entry := range strings.Split(string(content), "\x00") {
		if name, value, found := strings.Cut(entry, "="); found {
			variables[name] = value
		}
	}
	if len(variables) == 0 {
		t.Fatalf("%s holds no variables: that is not a process environment", *liveEnvironmentFile)
	}
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(variables))
	if err != nil {
		t.Fatalf("the new binary would refuse to start in this environment: %v", err)
	}
	if err := registry.CheckRequired(); err != nil {
		t.Fatalf("the new binary would refuse to start in this environment: %v", err)
	}
	t.Logf("a registry builds from the %d variables in %s", len(variables), *liveEnvironmentFile)
}
