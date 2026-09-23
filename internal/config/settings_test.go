package config

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
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func allowProductionAddresses(variables map[string]string) servicesettings.Environment {
	copied := map[string]string{productiondefaults.AllowInTestsEnvironmentVariable: "1"}
	for name, value := range variables {
		copied[name] = value
	}
	return servicesettings.MapEnvironment(copied)
}

func TestLoadRefusesAValueItCannotReadWhereItUsedToTakeTheDefault(t *testing.T) {
	t.Setenv(productiondefaults.AllowInTestsEnvironmentVariable, "1")
	for variable, value := range map[string]string{
		"LLMBRIDGE_IDLE_TIMEOUT":                "fifteen minutes",
		"LLMBRIDGE_SIGNAL_CLASSIFIER_MAX_CHARS": "lots",
		"LLMBRIDGE_PURPOSE_FOLDERS":             "herald=Reminders",
	} {
		_, err := LoadFrom(allowProductionAddresses(map[string]string{variable: value}))
		if err == nil || !strings.Contains(err.Error(), variable) {
			t.Errorf("%s=%q: LoadFrom = %v, want a refusal naming the variable", variable, value, err)
		}
	}
}

func TestLoadRefusesAnLLMBRIDGEVariableNobodyDeclared(t *testing.T) {
	t.Setenv(productiondefaults.AllowInTestsEnvironmentVariable, "1")
	_, err := LoadFrom(allowProductionAddresses(map[string]string{"LLMBRIDGE_GRANT_STORE_SERVICE_TOKEN": "the 2026-09-18 misspelling"}))
	if err == nil || !strings.Contains(err.Error(), "LLMBRIDGE_GRANT_STORE_SERVICE_TOKEN is set and llm-bridge-server declares no such setting") {
		t.Fatalf("LoadFrom = %v", err)
	}
	if _, err := LoadFrom(allowProductionAddresses(map[string]string{"GRANT_STORE_SERVICE_TOKEN": "x", "PATH": "/bin"})); err != nil {
		t.Errorf("a declared variable and one outside the prefix were refused: %v", err)
	}
}

func TestLoadReadsTheSameValuesItAlwaysDid(t *testing.T) {
	t.Setenv(productiondefaults.AllowInTestsEnvironmentVariable, "1")
	cfg, err := LoadFrom(allowProductionAddresses(map[string]string{
		"LLMBRIDGE_LISTEN_ADDR":                 ":9999",
		"LLMBRIDGE_IDLE_TIMEOUT":                "0s",
		"LLMBRIDGE_SIGNAL_CLASSIFIER_OPT_OUT":   "codex, aider",
		"LLMBRIDGE_PURPOSE_FOLDERS":             "herald:Nudges",
		"LLMBRIDGE_SIGNAL_CLASSIFIER_MAX_CHARS": "1200",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9999" || cfg.IdleTimeout != 0 || cfg.PTYIdleTimeout != time.Hour || cfg.SignalClassifierMaxChars != 1200 {
		t.Errorf("listen=%q idle=%v pty=%v max=%d", cfg.ListenAddr, cfg.IdleTimeout, cfg.PTYIdleTimeout, cfg.SignalClassifierMaxChars)
	}
	if !cfg.SignalClassifierOptOut["codex"] || !cfg.SignalClassifierOptOut["aider"] || len(cfg.SignalClassifierOptOut) != 2 {
		t.Errorf("opt out = %v", cfg.SignalClassifierOptOut)
	}
	if cfg.PurposeFolders["herald"] != "Nudges" {
		t.Errorf("the operator's pair did not land over the registry default: %v", cfg.PurposeFolders["herald"])
	}
	if cfg.SignalClassifierModel != "claude-haiku-4-5" || cfg.SignalClassifierInstance != "inst-cc-local" || cfg.SignalClassifierTimeout != 20*time.Second {
		t.Errorf("classifier defaults moved: %q %q %v", cfg.SignalClassifierModel, cfg.SignalClassifierInstance, cfg.SignalClassifierTimeout)
	}
	if err := cfg.Settings.CheckRequired(); err == nil || !strings.Contains(err.Error(), "LLMBRIDGE_PRINCIPAL_STORE_URL") {
		t.Errorf("CheckRequired = %v, want it to name the unset principal-store URL", err)
	}
}

func TestEverySecretTheServerStripsFromAChildIsDeclaredASecret(t *testing.T) {
	declaredSecrets := map[string]bool{}
	for _, definition := range SettingDefinitions() {
		if definition.Kind == msg.ServiceSettingKindSecret {
			declaredSecrets[definition.EnvironmentVariable] = true
		}
	}
	for _, name := range SecretEnvironmentVariableNames() {
		if !declaredSecrets[name] {
			t.Errorf("%s is stripped from child processes and is not declared a secret, so GET /settings would show its value", name)
		}
	}
}

func TestStoredSettingSeedsNameOnlyEditableSettings(t *testing.T) {
	editable := map[string]bool{}
	for _, definition := range SettingDefinitions() {
		if definition.Editable {
			editable[definition.Key] = true
		}
	}
	seeds := (&Config{SignalClassifierMaxChars: 10, OperationsCompletionInstance: "i", OperationsCompletionModel: "m", OperationsGrantEnforcement: "lenient"}).StoredSettingSeeds()
	for key := range seeds {
		if !editable[key] {
			t.Errorf("a seed names %s, which is not an editable setting", key)
		}
	}
	for key := range editable {
		if _, seeded := seeds[key]; !seeded {
			t.Errorf("editable setting %s has no seed from Config, so a Config literal cannot set it", key)
		}
	}
}

// Every environment variable the server's own code reads by name is declared.
// A read that is not declared is invisible on the settings page and escapes the
// startup check, which is the state this repo was in before 2026-09-18.
func TestEveryEnvironmentVariableTheServerReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range SettingDefinitions() {
		declared[definition.EnvironmentVariable] = true
	}
	// Read by name and not settings of this server.
	notSettings := map[string]bool{"HOME": true}

	repositoryRoot := filepath.Join("..", "..")
	for _, tree := range []string{"internal", filepath.Join("cmd", "llm-bridge-server")} {
		err := filepath.WalkDir(filepath.Join(repositoryRoot, tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
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
					return true // a computed name: its callers are held to the declarations by their own tests
				}
				name, _ := strconv.Unquote(literal.Value)
				if !declared[name] && !notSettings[name] {
					t.Errorf("%s reads %s, which SettingDefinitions does not declare", strings.TrimPrefix(path, repositoryRoot+string(os.PathSeparator)), name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
