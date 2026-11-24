package referencestore

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
)

// Every environment variable reference-store reads by name is declared. A read that is
// not declared is invisible on the settings page and escapes the startup check.
// healthcheck's repo-settings-guard runs this test by name every night.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range SettingDefinitions() {
		declared[definition.EnvironmentVariable] = true
	}
	// Read by name and not settings of this service.
	notSettings := map[string]bool{}
	// Directories that are not Go source, and directories whose package declares
	// its own settings and holds its own reads with its own copy of this test.
	skippedDirectories := map[string]bool{".git": true, "node_modules": true, "vendor": true}

	filesRead := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && (skippedDirectories[entry.Name()] || skippedDirectories[filepath.ToSlash(path)]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		filesRead++
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			packageName, isIdentifier := selector.X.(*ast.Ident)
			if !isIdentifier || packageName.Name != "os" {
				return true
			}
			switch selector.Sel.Name {
			case "Environ":
				t.Errorf("%s reads the whole environment, which no declaration can be held to", path)
				return true
			case "Getenv", "LookupEnv":
			default:
				return true
			}
			if len(call.Args) == 0 {
				return true
			}
			literal, isLiteral := call.Args[0].(*ast.BasicLit)
			if !isLiteral {
				t.Errorf("%s reads an environment variable whose name is computed, which no declaration can be held to", path)
				return true
			}
			name, _ := strconv.Unquote(literal.Value)
			if !declared[name] && !notSettings[name] {
				t.Errorf("%s reads %s, which the settings declarations do not name", path, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// If the package moves, the walk would read the wrong tree and pass.
	if _, err := os.Stat(filepath.FromSlash("cmd/reference-store/main.go")); err != nil {
		t.Fatalf("the scan starts somewhere other than it was written for: %v", err)
	}
	if filesRead < 4 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
