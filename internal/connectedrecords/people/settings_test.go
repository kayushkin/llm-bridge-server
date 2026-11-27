package people

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

// Every environment variable the package's code reads by name is declared by
// one of the importers. A read that is not declared is invisible on the
// settings page and escapes the startup check.
func TestEveryEnvironmentVariableThePackageReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range append(ImporterSettingDefinitions(), ContactsImporterSettingDefinitions()...) {
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
	if _, err := os.Stat(filepath.Join("cmd", "people-import-multichat", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the package directory: %v", err)
	}
	if filesRead < 5 {
		t.Fatalf("the scan read %d files; it is not looking at the package", filesRead)
	}
}
