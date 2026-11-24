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

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

const (
	aServiceToken           = "a-kanban-store-service-token-of-forty-chars"
	aGrantStoreServiceToken = "a-grant-store-service-token-of-forty-chars"
)

// requiredVariables is the least a kanban-store starts with: the three
// variables that have no default.
func requiredVariables(more map[string]string) map[string]string {
	variables := map[string]string{
		"KANBAN_STORE_SERVICE_TOKEN": aServiceToken,
		"GRANT_STORE_URL":            "http://grants.example:1",
		"GRANT_STORE_SERVICE_TOKEN":  aGrantStoreServiceToken,
	}
	for name, value := range more {
		variables[name] = value
	}
	return variables
}

func loadFrom(variables map[string]string) (Config, error) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(variables))
	if err != nil {
		return Config{}, err
	}
	if err := registry.CheckRequired(); err != nil {
		return Config{}, err
	}
	return Load(registry)
}

func mustLoadFrom(t *testing.T, variables map[string]string) Config {
	t.Helper()
	loaded, err := loadFrom(variables)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// The registry gives the command what os.Getenv gave it before 2026-09-20: the
// same defaults with only the required variables set, and the operator's values
// when they are.
func TestLoadReadsTheSameValuesItAlwaysDid(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	want := Config{
		ListenHost:             "127.0.0.1",
		ListenPort:             8305,
		DatabasePath:           "/home/someone/.kanban-store/kanban-store.db",
		NoteboardURL:           "http://localhost:8191",
		PrincipalStoreURL:      "http://127.0.0.1:8314",
		ReferenceStoreURL:      "http://127.0.0.1:8343",
		LLMBridgeServerURL:     "http://127.0.0.1:8160",
		BundleStoreURL:         "http://127.0.0.1:8307",
		FileStoreURL:           "",
		FileStoreServiceToken:  "",
		MultichatURL:           "",
		AuthStoreURL:           "http://127.0.0.1:8303",
		AuthStoreToken:         "",
		ServiceToken:           aServiceToken,
		GrantStoreURL:          "http://grants.example:1",
		GrantStoreServiceToken: aGrantStoreServiceToken,
	}
	if got := mustLoadFrom(t, requiredVariables(nil)); got != want {
		t.Errorf("Load with only the required variables set = %+v, want %+v", got, want)
	}

	want = Config{
		ListenHost:             "192.0.2.1",
		ListenPort:             9305,
		DatabasePath:           "/srv/kanban-store.db",
		NoteboardURL:           "http://notes.example:1",
		PrincipalStoreURL:      "http://principals.example:1",
		ReferenceStoreURL:      "http://references.example:1",
		LLMBridgeServerURL:     "http://bridge.example:1",
		BundleStoreURL:         "http://bundles.example:1",
		FileStoreURL:           "http://files.example:1",
		FileStoreServiceToken:  "a-file-store-token",
		MultichatURL:           "http://multichat.example:1",
		AuthStoreURL:           "http://auth.example:1",
		AuthStoreToken:         "an-auth-store-token",
		ServiceToken:           "another-kanban-store-service-token-of-some-length",
		GrantStoreURL:          "http://other-grants.example:1",
		GrantStoreServiceToken: "another-grant-store-service-token-of-some-length",
	}
	got := mustLoadFrom(t, map[string]string{
		"KANBAN_LISTEN_HOST":         "192.0.2.1",
		"KANBAN_PORT":                "9305",
		"KANBAN_DB":                  "/srv/kanban-store.db",
		"KANBAN_NOTEBOARD_URL":       "http://notes.example:1",
		"PRINCIPAL_STORE_URL":        "http://principals.example:1",
		"REFERENCE_STORE_URL":        "http://references.example:1",
		"LLM_BRIDGE_URL":             "http://bridge.example:1",
		"BUNDLE_STORE_URL":           "http://bundles.example:1",
		"FILE_STORE_URL":             "http://files.example:1",
		"FILE_STORE_SERVICE_TOKEN":   "a-file-store-token",
		"MULTICHAT_URL":              "http://multichat.example:1",
		"AUTH_STORE_URL":             "http://auth.example:1",
		"AUTH_STORE_TOKEN":           "an-auth-store-token",
		"KANBAN_STORE_SERVICE_TOKEN": "another-kanban-store-service-token-of-some-length",
		"GRANT_STORE_URL":            "http://other-grants.example:1",
		"GRANT_STORE_SERVICE_TOKEN":  "another-grant-store-service-token-of-some-length",
	})
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}

	// A variable set to the empty string is the same as unset, as it was when
	// main compared os.Getenv to "".
	got = mustLoadFrom(t, requiredVariables(map[string]string{"KANBAN_PORT": "", "KANBAN_DB": "", "KANBAN_NOTEBOARD_URL": "", "AUTH_STORE_URL": ""}))
	if got.ListenPort != 8305 || got.DatabasePath != "/home/someone/.kanban-store/kanban-store.db" || got.NoteboardURL != "http://localhost:8191" || got.AuthStoreURL != "http://127.0.0.1:8303" {
		t.Errorf("empty variables: %+v", got)
	}
}

// Every declared variable is set to a value of its own above. If a setting is
// added and that test is not told, this fails.
func TestTheSameValuesTestCoversEveryDeclaredSetting(t *testing.T) {
	if got, want := len(SettingDefinitions()), 16; got != want {
		t.Fatalf("%d settings are declared and TestLoadReadsTheSameValuesItAlwaysDid sets %d: add the new one there, then change this number", got, want)
	}
}

// Before 2026-09-20 KANBAN_PORT=eighty reached http.ListenAndServe as ":eighty"
// and failed there, after the database was open. Now the registry refuses it.
func TestAPortThatIsNotAWholeNumberIsRefused(t *testing.T) {
	_, err := loadFrom(requiredVariables(map[string]string{"KANBAN_PORT": "eighty"}))
	if err == nil || !strings.Contains(err.Error(), "KANBAN_PORT") {
		t.Fatalf("KANBAN_PORT=eighty = %v, want a refusal naming it", err)
	}
}

// Before 2026-09-20 an unknown home directory opened
// .kanban-store/kanban-store.db under the working directory, silently. Now the
// setting is unset and the command refuses to start.
func TestAnUnknownHomeDirectoryLeavesTheDatabaseUnsetAndCheckRequiredSaysSo(t *testing.T) {
	t.Setenv("HOME", "")
	_, err := loadFrom(requiredVariables(nil))
	if err == nil || !strings.Contains(err.Error(), "KANBAN_DB is unset") {
		t.Fatalf("with no home directory = %v, want a refusal naming KANBAN_DB", err)
	}
	if _, err := loadFrom(requiredVariables(map[string]string{"KANBAN_DB": "/srv/kanban-store.db"})); err != nil {
		t.Fatalf("with no home directory and KANBAN_DB set: %v", err)
	}
}

// There is no off switch for principal enforcement: each of the three
// variables is required, and a short token is as bad as none. A refusal names
// the variable and never quotes the token.
func TestPrincipalEnforcementVariablesAreRequiredAndARefusalNeverQuotesAToken(t *testing.T) {
	const shortToken = "short-secret-token"
	cases := []struct {
		name      string
		variables map[string]string
		wantNamed string
	}{
		{"no service token", map[string]string{"KANBAN_STORE_SERVICE_TOKEN": ""}, "KANBAN_STORE_SERVICE_TOKEN"},
		{"a short service token", map[string]string{"KANBAN_STORE_SERVICE_TOKEN": shortToken}, "KANBAN_STORE_SERVICE_TOKEN"},
		{"no grant-store address", map[string]string{"GRANT_STORE_URL": ""}, "GRANT_STORE_URL"},
		{"no grant-store token", map[string]string{"GRANT_STORE_SERVICE_TOKEN": ""}, "GRANT_STORE_SERVICE_TOKEN"},
		{"a short grant-store token", map[string]string{"GRANT_STORE_SERVICE_TOKEN": shortToken}, "GRANT_STORE_SERVICE_TOKEN"},
	}
	for _, c := range cases {
		_, err := loadFrom(requiredVariables(c.variables))
		if err == nil || !strings.Contains(err.Error(), c.wantNamed) {
			t.Errorf("%s: %v, want a refusal naming %s", c.name, err, c.wantNamed)
			continue
		}
		if strings.Contains(err.Error(), shortToken) {
			t.Errorf("%s: the refusal quotes the token: %v", c.name, err)
		}
	}
}

// An address whose owner takes no call without a token is refused without one,
// as main refused it before 2026-09-20. The check lives in Load so that the
// live-environment test deploy.sh runs asks it too.
func TestAnAddressSetWithoutItsTokenIsRefused(t *testing.T) {
	if _, err := loadFrom(requiredVariables(map[string]string{"FILE_STORE_URL": "http://files.example:1"})); err == nil || !strings.Contains(err.Error(), "FILE_STORE_SERVICE_TOKEN") {
		t.Errorf("FILE_STORE_URL alone = %v, want a refusal naming FILE_STORE_SERVICE_TOKEN", err)
	}
	if _, err := loadFrom(requiredVariables(map[string]string{"MULTICHAT_URL": "http://multichat.example:1"})); err == nil || !strings.Contains(err.Error(), "AUTH_STORE_TOKEN") {
		t.Errorf("MULTICHAT_URL alone = %v, want a refusal naming AUTH_STORE_TOKEN", err)
	}
	// A token with no address beside it starts: the shared token file carries
	// AUTH_STORE_TOKEN to services that never send a message.
	if _, err := loadFrom(requiredVariables(map[string]string{"AUTH_STORE_TOKEN": "an-auth-store-token", "FILE_STORE_SERVICE_TOKEN": "a-file-store-token"})); err != nil {
		t.Errorf("tokens with no address beside them were refused: %v", err)
	}
}

// KANBAN_STORE_URL is how other services find this one, and is not this
// service's to refuse; a misspelling of a declared name is.
func TestTheRegistryRefusesAMisspellingAndNotAVariableMeantForSomethingElse(t *testing.T) {
	for _, misspelled := range []string{"KANBAN_PORTS", "KANBAN_DB_PATH", "KANBAN_NOTEBOARD", "KANBAN_NOTEBOARD_ADDRESS", "KANBAN_STORE_SERVICE_TOKENS"} {
		_, err := NewSettingsRegistry(servicesettings.MapEnvironment(requiredVariables(map[string]string{misspelled: "x"})))
		if err == nil || !strings.Contains(err.Error(), misspelled+" is set and kanban-store declares no such setting") {
			t.Errorf("NewSettingsRegistry with %s = %v, want a refusal naming it", misspelled, err)
		}
	}
	notOurs := requiredVariables(map[string]string{
		"KANBAN_STORE_URL":        "http://localhost:8305",
		"KANBAN_URL":              "http://localhost:8305",
		"KANBAN_CLASSIFIER_MODEL": "a-model",
		"LLMBRIDGE_SERVICE_TOKEN": "x",
		"GRANT_STORE_AUDIT":       "x",
		"PATH":                    "/bin",
	})
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(notOurs)); err != nil {
		t.Errorf("variables meant for other services were refused: %v", err)
	}
}

// Every owned prefix is the start of a declared variable, and every declared
// KANBAN_ variable is under an owned prefix. An owned prefix that starts
// nothing guards nothing and reads as if it did.
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
		if !strings.HasPrefix(definition.EnvironmentVariable, "KANBAN_") {
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
	secrets := []string{}
	for _, definition := range SettingDefinitions() {
		if definition.Kind != msg.ServiceSettingKindSecret {
			continue
		}
		secrets = append(secrets, definition.EnvironmentVariable)
		if definition.ValueType != msg.ServiceSettingValueTypeString {
			t.Errorf("%s is a secret declared %s", definition.EnvironmentVariable, definition.ValueType)
		}
		if definition.Default != "" {
			t.Errorf("%s is a secret with a default", definition.EnvironmentVariable)
		}
	}
	if got, want := strings.Join(secrets, " "), "FILE_STORE_SERVICE_TOKEN AUTH_STORE_TOKEN KANBAN_STORE_SERVICE_TOKEN GRANT_STORE_SERVICE_TOKEN"; got != want {
		t.Errorf("secrets declared: %s, want %s", got, want)
	}
}

// Only GET /settings is mounted, so a setting declared Editable would promise
// a write nothing serves.
func TestNoSettingIsEditable(t *testing.T) {
	for _, definition := range SettingDefinitions() {
		if definition.Editable {
			t.Errorf("%s is declared Editable, and the service stores no setting and mounts no PUT /settings/{key}", definition.Key)
		}
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
	if _, err := os.Stat(filepath.Join(repositoryRoot, "cmd", "kanban-store", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 3 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
