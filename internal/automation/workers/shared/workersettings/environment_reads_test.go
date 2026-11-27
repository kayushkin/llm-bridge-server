package workersettings

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
)

// packageDirectoriesReachedFrom is start and every package of this module it
// imports, directly or through another.
func packageDirectoriesReachedFrom(t *testing.T, repositoryRoot, start string) []string {
	t.Helper()
	const module = "github.com/kayushkin/llm-bridge-server/internal/automation/"
	seen := map[string]bool{}
	queue := []string{start}
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

// notSettings are variables a job binary's code reads by name that configure
// nothing: demo-mail-generator/demomail puts PATH into an error message when gh is not on
// it.
var notSettings = []string{"PATH"}

// Every environment variable a job binary reads by name, in workers/<name>/cmd/<name> or any
// package it imports, is one that binary declares. A read that is not declared
// escapes the startup check, and the binary's misspelling guard with it.
func TestEveryEnvironmentVariableAJobBinaryReadsIsDeclared(t *testing.T) {
	// The scan covers internal/automation: the workers and any scheduler
	// package one of them imports.
	repositoryRoot := filepath.Join("..", "..", "..")
	if _, err := os.Stat(filepath.Join(repositoryRoot, "scheduler", "cron", "cron.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not internal/automation: %v", err)
	}
	for _, binary := range JobBinaries {
		declared := map[string]bool{}
		for _, definition := range binary.SettingDefinitions() {
			declared[definition.EnvironmentVariable] = true
		}
		for _, name := range notSettings {
			declared[name] = true
		}
		directories := packageDirectoriesReachedFrom(t, repositoryRoot, "workers/"+binary.Name+"/cmd/"+binary.Name)
		// Every binary builds its registry from this package, so a walk that
		// does not reach it is not following imports.
		if !contains(directories, "workers/shared/workersettings") {
			t.Errorf("%s: the scan does not reach workersettings; it reached %v", binary.Name, directories)
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
					t.Errorf("%s: %s %s", binary.Name, path, fault)
				}
			}
		}
		if filesRead < 3 {
			t.Errorf("%s: the scan read %d files; it is not looking at the binary", binary.Name, filesRead)
		}
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
			faults = append(faults, "reads "+name+", which its SettingDefinitions do not declare")
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
