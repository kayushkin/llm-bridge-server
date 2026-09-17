package childprocessenv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
)

func TestRemoveServerSecretsDropsEverySecretAndKeepsEverythingElse(t *testing.T) {
	environment := []string{
		"PATH=/usr/bin",
		"LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY=signing-key",
		"LLMBRIDGE_SERVICE_TOKEN=service-token",
		"LLMBRIDGE_GRANT_STORE_SERVICE_TOKEN=grant-token",
		"LLMBRIDGE_KANBAN_STORE_SERVICE_TOKEN=kanban-token",
		"LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY=a-duplicate-entry",
		// Near-misses are not secrets and must survive: names are exact.
		"LLMBRIDGE_SERVICE_TOKEN_HINT=not-a-secret",
		"llmbridge_service_token=different-name-on-unix",
		"LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY_FILE=/etc/keys/login",
		"NO_EQUALS_SIGN",
	}
	got := RemoveServerSecrets(environment)
	want := []string{
		"PATH=/usr/bin",
		"LLMBRIDGE_SERVICE_TOKEN_HINT=not-a-secret",
		"llmbridge_service_token=different-name-on-unix",
		"LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY_FILE=/etc/keys/login",
		"NO_EQUALS_SIGN",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("RemoveServerSecrets =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestEveryDeclaredSecretIsRemovedFromTheProcessEnvironment(t *testing.T) {
	for _, name := range config.SecretEnvironmentVariableNames() {
		t.Setenv(name, "secret-value-for-"+name)
	}
	t.Setenv("LLMBRIDGE_CHILD_ENVIRONMENT_TEST_MARKER", "kept")
	environment := EnvironmentWithoutServerSecrets()
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		for _, secretName := range config.SecretEnvironmentVariableNames() {
			if name == secretName {
				t.Errorf("child environment still carries %s", secretName)
			}
		}
	}
	found := false
	for _, entry := range environment {
		if entry == "LLMBRIDGE_CHILD_ENVIRONMENT_TEST_MARKER=kept" {
			found = true
		}
	}
	if !found {
		t.Errorf("child environment lost a variable that is not a secret")
	}
}

// TestEverySpawnInTheModuleSetsItsEnvironmentFromThisPackage is a regression
// guard, not the proof: the proof is the real spawns in the harness and server
// packages' tests. An exec.Cmd whose Env is never assigned inherits the full
// server environment, secrets included, so every exec.Command/CommandContext
// call in non-test code must be followed in the same function by an
// assignment to that command's Env whose right-hand side calls into this
// package — and os.Environ / Cmd.Environ must not appear outside it.
func TestEverySpawnInTheModuleSetsItsEnvironmentFromThisPackage(t *testing.T) {
	moduleRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	err = filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || strings.HasPrefix(name, ".") && path != moduleRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Dir(path) == filepath.Join(moduleRoot, "internal", "childprocessenv") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fileSet, path, source, 0)
		if err != nil {
			return err
		}
		checkFileSpawnsWithScrubbedEnvironment(t, fileSet, file, source)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkFileSpawnsWithScrubbedEnvironment(t *testing.T, fileSet *token.FileSet, file *ast.File, source []byte) {
	t.Helper()
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if selector.Sel.Name == "Environ" {
			t.Errorf("%s: calls %s — build a child environment with childprocessenv.EnvironmentWithoutServerSecrets instead",
				fileSet.Position(call.Pos()), string(source[call.Pos()-1:call.End()-1]))
		}
		return true
	})
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		commandVariables := map[string]token.Pos{}
		scrubbedVariables := map[string]bool{}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for index, right := range assignment.Rhs {
				if index >= len(assignment.Lhs) {
					break
				}
				if call, ok := right.(*ast.CallExpr); ok {
					if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
						if identifier, ok := selector.X.(*ast.Ident); ok && identifier.Name == "exec" &&
							(selector.Sel.Name == "Command" || selector.Sel.Name == "CommandContext") {
							if left, ok := assignment.Lhs[index].(*ast.Ident); ok {
								commandVariables[left.Name] = call.Pos()
							}
						}
					}
				}
				if left, ok := assignment.Lhs[index].(*ast.SelectorExpr); ok && left.Sel.Name == "Env" {
					if receiver, ok := left.X.(*ast.Ident); ok && callsChildProcessEnvironmentPackage(right) {
						scrubbedVariables[receiver.Name] = true
					}
				}
			}
			return true
		})
		for variable, position := range commandVariables {
			if !scrubbedVariables[variable] {
				t.Errorf("%s: %s is built by exec.Command but its Env is never set from childprocessenv, so it inherits the server's secrets",
					fileSet.Position(position), variable)
			}
		}
	}
}

func callsChildProcessEnvironmentPackage(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if identifier, ok := selector.X.(*ast.Ident); ok && identifier.Name == "childprocessenv" {
				found = true
			}
		}
		return !found
	})
	return found
}
