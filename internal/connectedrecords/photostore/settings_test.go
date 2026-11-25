package photostore

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

func TestTheRegistryReadsDefaultsAndRefusesAnUndeclaredVariable(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.String(SettingListenAddress); got != "127.0.0.1:8329" {
		t.Errorf("listen address = %q, want loopback 127.0.0.1:8329", got)
	}
	if got := registry.String(SettingDataDirectory); got != DefaultDataDir() {
		t.Errorf("data directory = %q, want %q", got, DefaultDataDir())
	}
	_, err = NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"PHOTO_STORE_ADDRESS": ":1"}))
	if err == nil || !strings.Contains(err.Error(), "PHOTO_STORE_ADDRESS") {
		t.Fatalf("an undeclared PHOTO_STORE_ variable = %v, want a refusal naming it", err)
	}
}

// Every environment variable the service's own code reads by name is declared.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range append(append(append(SettingDefinitions(), ImportMessagesSettingDefinitions()...), ImportFacebookSettingDefinitions()...), PeopleOnPhotosSettingDefinitions()...) {
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
				t.Errorf("%s reads an environment variable whose name is computed", path)
				return true
			}
			if name, _ := strconv.Unquote(literal.Value); !declared[name] {
				t.Errorf("%s reads %s, which SettingDefinitions does not declare", path, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join("cmd", "photo-store", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 5 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
