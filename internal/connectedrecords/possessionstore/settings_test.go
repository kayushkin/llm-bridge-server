package possessionstore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

func TestTheAddPhotosRegistryNeedsTheBridgeAndAToken(t *testing.T) {
	registry, err := NewAddPhotosSettingsRegistry(servicesettings.MapEnvironment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err == nil || !strings.Contains(err.Error(), "LLM_BRIDGE_GATEWAY_URL") {
		t.Fatalf("no bridge named = %v, want a refusal naming LLM_BRIDGE_GATEWAY_URL", err)
	}
	registry, err = NewAddPhotosSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"LLM_BRIDGE_GATEWAY_URL": "http://127.0.0.1:1", "LLM_BRIDGE_PRINCIPAL_TOKEN": "made-up"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err != nil {
		t.Fatal(err)
	}
}

// Every environment variable the code reads by name is declared.
func TestEveryEnvironmentVariableTheCodeReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range AddPhotosSettingDefinitions() {
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
