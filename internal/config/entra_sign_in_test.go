package config

import (
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

func TestDemoLoginIsOnUnlessTheSettingSaysOtherwise(t *testing.T) {
	for value, want := range map[string]bool{"": true, "true": true, "false": false} {
		environment := map[string]string{}
		if value != "" {
			environment["LLMBRIDGE_DEMO_LOGIN_ENABLED"] = value
		}
		registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(environment))
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if got := registry.Boolean("demo_login.enabled"); got != want {
			t.Errorf("LLMBRIDGE_DEMO_LOGIN_ENABLED=%q reads %v, want %v", value, got, want)
		}
	}
}

func completeEntraConfig() Config {
	return Config{
		EntraTenantID:     "72F988BF-86F1-41AF-91AB-2D7CD011DB47",
		EntraClientID:     "11111111-2222-3333-4444-555555555555",
		EntraClientSecret: "secret",
		EntraRedirectURL:  "https://bridge.example.test/auth/entra/callback",
		EntraAuthorityURL: "https://login.microsoftonline.com/",
	}
}

func TestEntraSignInIsAllFourSettingsOrNone(t *testing.T) {
	none := Config{EntraAuthorityURL: "https://login.microsoftonline.com"}
	if err := none.ValidateEntraSignInSettings(); err != nil || none.EntraSignInEnabled() {
		t.Fatalf("no Entra settings: %v, enabled %v; want off and no error", err, none.EntraSignInEnabled())
	}
	complete := completeEntraConfig()
	if err := complete.ValidateEntraSignInSettings(); err != nil || !complete.EntraSignInEnabled() {
		t.Fatalf("complete: %v, enabled %v", err, complete.EntraSignInEnabled())
	}
	if got := complete.EntraIssuerURL(); got != "https://login.microsoftonline.com/72f988bf-86f1-41af-91ab-2d7cd011db47/v2.0" {
		t.Fatalf("issuer = %q", got)
	}
	for name, testCase := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"no secret":          {func(c *Config) { c.EntraClientSecret = "" }, EntraClientSecretEnvironmentVariable},
		"no redirect":        {func(c *Config) { c.EntraRedirectURL = "" }, "LLMBRIDGE_ENTRA_REDIRECT_URL"},
		"only tenant":        {func(c *Config) { c.EntraClientID, c.EntraClientSecret, c.EntraRedirectURL = "", "", "" }, "LLMBRIDGE_ENTRA_CLIENT_ID"},
		"tenant not a GUID":  {func(c *Config) { c.EntraTenantID = "contoso.onmicrosoft.com" }, "not a GUID"},
		"tenant is common":   {func(c *Config) { c.EntraTenantID = "common" }, "not a GUID"},
		"relative redirect":  {func(c *Config) { c.EntraRedirectURL = "/auth/entra/callback" }, "LLMBRIDGE_ENTRA_REDIRECT_URL"},
		"no authority":       {func(c *Config) { c.EntraAuthorityURL = "" }, "LLMBRIDGE_ENTRA_AUTHORITY_URL"},
		"authority a scheme": {func(c *Config) { c.EntraAuthorityURL = "login.microsoftonline.com" }, "LLMBRIDGE_ENTRA_AUTHORITY_URL"},
	} {
		candidate := completeEntraConfig()
		testCase.mutate(&candidate)
		err := candidate.ValidateEntraSignInSettings()
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: %v, want an error naming %s", name, err, testCase.want)
		}
	}
}

func TestTheEntraClientSecretNeverReachesAChildProcess(t *testing.T) {
	for _, name := range SecretEnvironmentVariableNames() {
		if name == "LLMBRIDGE_ENTRA_CLIENT_SECRET" {
			return
		}
	}
	t.Fatal("LLMBRIDGE_ENTRA_CLIENT_SECRET is not in SecretEnvironmentVariableNames")
}
