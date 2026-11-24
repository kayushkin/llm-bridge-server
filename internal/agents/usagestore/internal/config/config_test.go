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

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func loadFrom(t *testing.T, variables map[string]string) Config {
	t.Helper()
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(variables))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(registry)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// The registry gives the command what envOr and envDur gave it before
// 2026-09-20: the same defaults with nothing set, and the operator's values
// when they are.
func TestLoadReadsTheSameValuesItAlwaysDid(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	want := Config{
		ListenAddr:                    ":8185",
		LimitsDBPath:                  "/home/someone/.config/usage-store/usage.db",
		TokensDBPath:                  "/home/someone/.config/model-store/store.db",
		LimitsIdleRefreshInterval:     15 * time.Minute,
		LimitsWatchedRefreshInterval:  2 * time.Minute,
		LimitsWatcherExpiry:           3 * time.Minute,
		LimitsStaleAfterIdleIntervals: 3,
		CodexCommand:                  "",
		CodexTimeout:                  30 * time.Second,
		AuthStoreURL:                  "http://127.0.0.1:8303",
		AuthStoreToken:                "",
		SpendRefreshInterval:          time.Hour,
	}
	if got := loadFrom(t, map[string]string{}); got != want {
		t.Errorf("Load with nothing set = %+v, want %+v", got, want)
	}

	want = Config{
		ListenAddr:                    "127.0.0.1:9999",
		LimitsDBPath:                  "/srv/usage.db",
		TokensDBPath:                  "/home/someone/tokens.db",
		LimitsIdleRefreshInterval:     20 * time.Minute,
		LimitsWatchedRefreshInterval:  90 * time.Second,
		LimitsWatcherExpiry:           4 * time.Minute,
		LimitsStaleAfterIdleIntervals: 3,
		CodexCommand:                  "/usr/local/bin/codex",
		CodexTimeout:                  5 * time.Second,
		AuthStoreURL:                  "http://auth.example:1",
		AuthStoreToken:                "a-token",
		SpendRefreshInterval:          30 * time.Minute,
	}
	got := loadFrom(t, map[string]string{
		"USAGE_STORE_LISTEN_ADDR":                     "127.0.0.1:9999",
		"USAGE_STORE_DB":                              "/srv/usage.db",
		"USAGE_STORE_TOKENS_DB":                       "~/tokens.db",
		"USAGE_STORE_LIMITS_IDLE_REFRESH_INTERVAL":    "20m",
		"USAGE_STORE_LIMITS_WATCHED_REFRESH_INTERVAL": "90s",
		"USAGE_STORE_LIMITS_WATCHER_EXPIRY":           "4m",
		"USAGE_STORE_CODEX_COMMAND":                   "/usr/local/bin/codex",
		"USAGE_STORE_CODEX_TIMEOUT":                   "5s",
		"AUTH_STORE_URL":                              "http://auth.example:1",
		"AUTH_STORE_TOKEN":                            "a-token",
		"USAGE_STORE_SPEND_REFRESH_INTERVAL":          "30m",
	})
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}

	// A variable set to the empty string is the same as unset, as it was when
	// envOr and envDur compared os.Getenv to "".
	got = loadFrom(t, map[string]string{"USAGE_STORE_LISTEN_ADDR": "", "USAGE_STORE_CODEX_TIMEOUT": "", "AUTH_STORE_URL": ""})
	if got.ListenAddr != ":8185" || got.CodexTimeout != 30*time.Second || got.AuthStoreURL != "http://127.0.0.1:8303" {
		t.Errorf("empty variables: %+v", got)
	}
}

// Every declared variable is set to a value of its own above. If a setting is
// added and that test is not told, this fails.
func TestTheSameValuesTestCoversEveryDeclaredSetting(t *testing.T) {
	if got, want := len(SettingDefinitions()), 11; got != want {
		t.Fatalf("%d settings are declared and TestLoadReadsTheSameValuesItAlwaysDid sets %d: add the new one there, then change this number", got, want)
	}
}

// Before 2026-09-20 envDur dropped a duration it could not parse and used the
// default without a word, so a typo in the unit changed nothing and logged
// nothing. Now the service refuses to start and names the variable.
func TestADurationThatDoesNotParseIsRefused(t *testing.T) {
	for _, definition := range SettingDefinitions() {
		if definition.ValueType != msg.ServiceSettingValueTypeDuration {
			continue
		}
		_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{definition.EnvironmentVariable: "15 minutes"}))
		if err == nil || !strings.Contains(err.Error(), definition.EnvironmentVariable) {
			t.Errorf("NewSettingsRegistry with %s=\"15 minutes\" = %v, want a refusal naming it", definition.EnvironmentVariable, err)
		}
	}
}

// Before 2026-09-20 an unknown home directory opened /.config/usage-store/usage.db,
// silently. Now the setting is unset and the command refuses to start.
func TestAnUnknownHomeDirectoryLeavesTheDatabasesUnsetAndCheckRequiredSaysSo(t *testing.T) {
	t.Setenv("HOME", "")
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	err = registry.CheckRequired()
	if err == nil || !strings.Contains(err.Error(), "USAGE_STORE_DB is unset") || !strings.Contains(err.Error(), "USAGE_STORE_TOKENS_DB is unset") {
		t.Fatalf("CheckRequired = %v, want it to name USAGE_STORE_DB and USAGE_STORE_TOKENS_DB", err)
	}

	// An operator's ~ cannot be expanded either, and Load says which variable.
	registry, err = NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"USAGE_STORE_DB": "~/usage.db", "USAGE_STORE_TOKENS_DB": "/srv/tokens.db"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(registry); err == nil || !strings.Contains(err.Error(), "USAGE_STORE_DB") {
		t.Fatalf("Load with ~ and no home directory = %v, want an error naming USAGE_STORE_DB", err)
	}
}

// USAGE_STORE_URL is how other services find this one, and is not this
// service's to refuse; a misspelling of a declared name is.
func TestTheRegistryRefusesAMisspellingAndNotAVariableMeantForSomethingElse(t *testing.T) {
	for _, misspelled := range []string{
		"USAGE_STORE_LISTEN_ADDRESS", "USAGE_STORE_DB_PATH", "USAGE_STORE_TOKENS_DB_PATH",
		"USAGE_STORE_LIMITS_REFRESH_INTERVAL", "USAGE_STORE_CODEX_CMD", "USAGE_STORE_SPEND_INTERVAL",
	} {
		_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{misspelled: "x"}))
		if err == nil || !strings.Contains(err.Error(), misspelled+" is set and usage-store declares no such setting") {
			t.Errorf("NewSettingsRegistry with %s = %v, want a refusal naming it", misspelled, err)
		}
	}
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"USAGE_STORE_URL": "http://localhost:8185", "USAGE_STORE_LISTEN_ADDR": ":1", "AUTH_STORE_AUDIT": "x", "PATH": "/bin"})); err != nil {
		t.Errorf("USAGE_STORE_URL beside a declared variable was refused: %v", err)
	}
}

// Every owned prefix is the start of a declared variable. One that is not
// guards nothing and reads as if it did.
func TestEveryOwnedPrefixStartsADeclaredVariable(t *testing.T) {
	for _, prefix := range OwnedEnvironmentVariablePrefixes {
		found := false
		for _, definition := range SettingDefinitions() {
			found = found || strings.HasPrefix(definition.EnvironmentVariable, prefix)
		}
		if !found {
			t.Errorf("owned prefix %s starts no declared variable", prefix)
		}
	}
	for _, definition := range SettingDefinitions() {
		if !strings.HasPrefix(definition.EnvironmentVariable, "USAGE_STORE_") {
			continue
		}
		owned := false
		for _, prefix := range OwnedEnvironmentVariablePrefixes {
			owned = owned || strings.HasPrefix(definition.EnvironmentVariable, prefix)
		}
		if !owned {
			t.Errorf("%s is under no owned prefix, so a misspelling of it starts the service without a word", definition.EnvironmentVariable)
		}
	}
}

// New quotes a value it cannot parse. A secret must be a string, which always
// parses, or its value could reach the log through its own refusal.
func TestEverySecretIsAString(t *testing.T) {
	secrets := 0
	for _, definition := range SettingDefinitions() {
		if definition.Kind != msg.ServiceSettingKindSecret {
			continue
		}
		secrets++
		if definition.ValueType != msg.ServiceSettingValueTypeString {
			t.Errorf("%s is a secret declared %s", definition.EnvironmentVariable, definition.ValueType)
		}
	}
	if secrets != 1 {
		t.Errorf("%d secrets declared, want AUTH_STORE_TOKEN alone", secrets)
	}
}

// Every environment variable the service's own code reads by name is declared.
// A read that is not declared is invisible on the settings page and escapes the
// startup check.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range SettingDefinitions() {
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
				t.Errorf("%s reads %s, which SettingDefinitions does not declare", path, name)
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
	if _, err := os.Stat(filepath.Join(repositoryRoot, "cmd", "usage-store-server", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 3 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
