package evalstore

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func TestTheListenAddressHasNoDefaultAndServeRequiresIt(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"HOME": "/home/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.String(SettingListenAddress); got != "" {
		t.Errorf("listen address with nothing set = %q; the port belongs in the unit, not the code", got)
	}
	if err := registry.CheckRequired(); err == nil || !strings.Contains(err.Error(), "EVAL_STORE_ADDR") {
		t.Errorf("CheckRequired = %v, want it to name EVAL_STORE_ADDR", err)
	}
	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"EVAL_STORE_ADDR": "127.0.0.1:1"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := set.CheckRequired(); err != nil {
		t.Errorf("CheckRequired with the address set = %v", err)
	}
	if set.Integer(SettingDefaultConcurrency) != 4 || set.Integer(SettingBudgetCheckEveryCalls) != 8 {
		t.Errorf("defaults: concurrency %d, budget check every %d", set.Integer(SettingDefaultConcurrency), set.Integer(SettingBudgetCheckEveryCalls))
	}
	limits := set.StringMap(SettingDefaultBudgetLimits)
	if limits["codex.weekly"] != "60" || limits["anthropic.seven_day"] != "40" {
		t.Errorf("default budget limits = %v, want codex.weekly 60 and anthropic.seven_day 40", limits)
	}
}

func TestTheRegistryRefusesAnEvalStoreVariableNobodyDeclared(t *testing.T) {
	_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"EVAL_STORE_ADDRESS": ":1"}))
	if err == nil || !strings.Contains(err.Error(), "EVAL_STORE_ADDRESS") {
		t.Fatalf("NewSettingsRegistry = %v, want a refusal naming the misspelled variable", err)
	}
}

func TestGetSettingsDescribesTheServiceHidesTheTokenAndNothingCanBeWritten(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"EVAL_STORE_ADDR": ":9", "LLMBRIDGE_SERVICE_TOKEN": "synthetic-token-value"}))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterSettingsHandler(mux, registry)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "synthetic-token-value") {
		t.Fatal("GET /settings shows the service token")
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and there is no operator gate", setting.Key)
		}
	}
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/settings/"+SettingListenAddress, strings.NewReader(`{"value":":1"}`)))
	if recorder.Code == http.StatusOK {
		t.Error("a settings write route is mounted")
	}
}

// Every environment variable the code reads by name is declared.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range SettingDefinitions() {
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
			name, _ := strconv.Unquote(literal.Value)
			if !declared[name] {
				t.Errorf("%s reads %s, which SettingDefinitions does not declare", path, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("go.mod"); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 5 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
