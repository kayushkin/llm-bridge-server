package mailstack

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
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/localmail"
)

// testSettings is a registry whose only set variable is the bearer token, which
// is what a test server needs from it.
func testSettings(t *testing.T, bearerToken string) *servicesettings.Registry {
	t.Helper()
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"MAILSTACK_TOKEN": bearerToken}))
	if err != nil {
		t.Fatalf("build settings: %v", err)
	}
	return registry
}

// The registry gives the command what its six environment reads gave it before
// 2026-09-20: port 8195, the two default database paths, auth-store on
// 127.0.0.1:8303 and no tokens with nothing set, and the operator's values
// when they are.
func TestTheRegistryReadsTheSameValuesTheCommandAlwaysDid(t *testing.T) {
	unset, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"HOME": "/home/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := unset.Integer(SettingListenPort); got != 8195 {
		t.Errorf("listen port with nothing set = %d, want 8195", got)
	}
	if got := unset.String(SettingAccountsDatabasePath); got != accounts.DefaultDBPath() {
		t.Errorf("accounts database with nothing set = %q, want accounts.DefaultDBPath(), %q", got, accounts.DefaultDBPath())
	}
	if got := unset.String(SettingLocalMailDatabasePath); got != localmail.DefaultDBPath() {
		t.Errorf("local mail database with nothing set = %q, want what the server opens for an empty path, %q", got, localmail.DefaultDBPath())
	}
	if got := unset.String(SettingAuthStoreURL); got != "http://127.0.0.1:8303" {
		t.Errorf("auth-store URL with nothing set = %q, want http://127.0.0.1:8303", got)
	}
	if unset.String(SettingBearerToken) != "" || unset.String(SettingAuthStoreBearerToken) != "" {
		t.Errorf("a token has a value with nothing set")
	}

	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"MAILSTACK_PORT":     "9999",
		"MAILSTACK_DB":       "/srv/mail/accounts.db",
		"MAILSTACK_LOCAL_DB": "/srv/mail/local.db",
		"MAILSTACK_TOKEN":    "mailstack-bearer",
		"AUTH_STORE_URL":     "http://127.0.0.1:1",
		"AUTH_STORE_TOKEN":   "auth-store-bearer",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Integer(SettingListenPort); got != 9999 {
		t.Errorf("listen port = %d", got)
	}
	for key, want := range map[string]string{
		SettingAccountsDatabasePath:  "/srv/mail/accounts.db",
		SettingLocalMailDatabasePath: "/srv/mail/local.db",
		SettingBearerToken:           "mailstack-bearer",
		SettingAuthStoreURL:          "http://127.0.0.1:1",
		SettingAuthStoreBearerToken:  "auth-store-bearer",
	} {
		if got := set.String(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// A variable set to the empty string is the same as unset, as it was when
	// the command compared os.Getenv to "". scripts/e2e-smoke.sh starts the
	// server with AUTH_STORE_TOKEN="".
	empty, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"MAILSTACK_PORT": "", "MAILSTACK_DB": "", "AUTH_STORE_URL": "", "AUTH_STORE_TOKEN": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Integer(SettingListenPort) != 8195 || empty.String(SettingAccountsDatabasePath) != accounts.DefaultDBPath() || empty.String(SettingAuthStoreURL) != "http://127.0.0.1:8303" {
		t.Errorf("empty variables: port=%d db=%q auth-store=%q", empty.Integer(SettingListenPort), empty.String(SettingAccountsDatabasePath), empty.String(SettingAuthStoreURL))
	}
}

// Before 2026-09-20 a MAILSTACK_PORT that was not a number was dropped without
// a word and the service listened on 8195. It now stops the start.
func TestAPortThatIsNotANumberStopsTheStart(t *testing.T) {
	_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"MAILSTACK_PORT": "eighty"}))
	if err == nil || !strings.Contains(err.Error(), "MAILSTACK_PORT") {
		t.Fatalf("NewSettingsRegistry = %v, want a refusal naming MAILSTACK_PORT", err)
	}
}

func TestTheRegistryRefusesAMailstackVariableNobodyDeclared(t *testing.T) {
	for _, misspelled := range []string{"MAILSTACK_DB_PATH", "MAILSTACK_TOKENS", "MAILSTACK_PORTS", "MAILSTACK_LOCAL_DB_PATH"} {
		_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{misspelled: "secret-looking-value"}))
		if err == nil || !strings.Contains(err.Error(), misspelled+" is set and mailstack declares no such setting") {
			t.Errorf("%s: NewSettingsRegistry = %v, want a refusal naming it", misspelled, err)
			continue
		}
		if strings.Contains(err.Error(), "secret-looking-value") {
			t.Errorf("%s: the refusal quotes the variable's value: %v", misspelled, err)
		}
	}
	// MAILSTACK_URL is what callers read to find this service. It is theirs,
	// and a shared environment file that carries it must not stop the service.
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"MAILSTACK_URL": "http://localhost:8195", "MAILSTACK_PORT": "8195", "PATH": "/bin"})); err != nil {
		t.Errorf("MAILSTACK_URL beside a declared variable was refused: %v", err)
	}
}

func TestGetSettingsSitsBehindTheBearerAndNeverServesAToken(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"MAILSTACK_PORT":   "9999",
		"MAILSTACK_TOKEN":  "mailstack-bearer-text",
		"AUTH_STORE_TOKEN": "auth-store-bearer-text",
	}))
	if err != nil {
		t.Fatal(err)
	}
	store, err := accounts.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := NewServer(store, nil, registry)

	get := func(authorization string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/settings", nil)
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	if code := get("").Code; code != http.StatusUnauthorized {
		t.Errorf("GET /settings with no bearer = %d, want 401", code)
	}
	if code := get("Bearer wrong").Code; code != http.StatusUnauthorized {
		t.Errorf("GET /settings with the wrong bearer = %d, want 401", code)
	}
	recorder := get("Bearer mailstack-bearer-text")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d: %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "mailstack-bearer-text") || strings.Contains(body, "auth-store-bearer-text") {
		t.Fatalf("GET /settings serves a token's text")
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != ServiceName || len(described.Settings) != len(SettingDefinitions()) {
		t.Fatalf("service=%q with %d settings, want %q with %d", described.Service, len(described.Settings), ServiceName, len(SettingDefinitions()))
	}
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and the service stores no setting", setting.Key)
		}
		if setting.Key == SettingListenPort && (setting.Value != "9999" || setting.Source != msg.ServiceSettingSourceEnvironment) {
			t.Errorf("listen port served as %q from %q", setting.Value, setting.Source)
		}
	}

	request := httptest.NewRequest(http.MethodPut, "/settings/"+SettingListenPort, strings.NewReader(`{"value":"1"}`))
	request.Header.Set("Authorization", "Bearer mailstack-bearer-text")
	put := httptest.NewRecorder()
	server.Handler().ServeHTTP(put, request)
	if put.Code == http.StatusOK {
		t.Errorf("PUT /settings/%s = 200: a write route is mounted", SettingListenPort)
	}
	if got := registry.Integer(SettingListenPort); got != 9999 {
		t.Errorf("the refused write changed the listen port to %d", got)
	}
}

func TestNewServerRefusesToBeBuiltWithoutItsSettings(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer(nil settings) returned: GET /settings would fail at its first read instead of at boot")
		}
	}()
	NewServer(nil, nil, nil)
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
	// The walk starts at the package directory, which is the repository root. If
	// the package moves, the walk would read nothing and pass.
	if _, err := os.Stat(filepath.Join("cmd", "mailstack", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 3 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
