package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// globallyUniqueIdentifierPattern is the shape of an Entra tenant id or object
// id: 8-4-4-4-12 hexadecimal digits.
var globallyUniqueIdentifierPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsGloballyUniqueIdentifier reports whether value is a GUID, the shape Entra
// gives tenant ids (tid) and object ids (oid).
func IsGloballyUniqueIdentifier(value string) bool {
	return globallyUniqueIdentifierPattern.MatchString(value)
}

// EntraSignInEnabled reports whether every setting Entra sign-in needs is set.
// ValidateEntraSignInSettings is what refuses a partial set; this answers only
// for a config that passed it.
func (c *Config) EntraSignInEnabled() bool {
	return c.EntraTenantID != "" && c.EntraClientID != "" && c.EntraClientSecret != "" && c.EntraRedirectURL != ""
}

// ValidateEntraSignInSettings accepts none of the four Entra settings, or all
// four well formed, and refuses anything between, naming what is missing. A
// partial set is a mistake in the unit, and starting with sign-in silently
// off would hide it until someone failed to sign in.
//
// main calls this and refuses to start on an error; server.New calls it too.
func (c *Config) ValidateEntraSignInSettings() error {
	settings := []struct {
		environmentVariable string
		value               string
	}{
		{"LLMBRIDGE_ENTRA_TENANT_ID", c.EntraTenantID},
		{"LLMBRIDGE_ENTRA_CLIENT_ID", c.EntraClientID},
		{EntraClientSecretEnvironmentVariable, c.EntraClientSecret},
		{"LLMBRIDGE_ENTRA_REDIRECT_URL", c.EntraRedirectURL},
	}
	var missing, present []string
	for _, setting := range settings {
		if strings.TrimSpace(setting.value) == "" {
			missing = append(missing, setting.environmentVariable)
		} else {
			present = append(present, setting.environmentVariable)
		}
	}
	if len(present) == 0 {
		return nil
	}
	if len(missing) > 0 {
		return fmt.Errorf("Entra sign-in is half configured: %s set but %s unset; set all four to switch it on, or none to leave it off",
			strings.Join(present, ", "), strings.Join(missing, ", "))
	}
	if !IsGloballyUniqueIdentifier(c.EntraTenantID) {
		return fmt.Errorf("LLMBRIDGE_ENTRA_TENANT_ID %q is not a GUID; it must be the tenant's id, not a domain name or \"common\", because the issuer a token is checked against is built from it", c.EntraTenantID)
	}
	if err := requireAbsoluteHTTPURL(c.EntraRedirectURL); err != nil {
		return fmt.Errorf("LLMBRIDGE_ENTRA_REDIRECT_URL: %w", err)
	}
	if strings.TrimSpace(c.EntraAuthorityURL) == "" {
		return fmt.Errorf("LLMBRIDGE_ENTRA_AUTHORITY_URL is empty; it is https://login.microsoftonline.com unless a test points it elsewhere")
	}
	if err := requireAbsoluteHTTPURL(c.EntraAuthorityURL); err != nil {
		return fmt.Errorf("LLMBRIDGE_ENTRA_AUTHORITY_URL: %w", err)
	}
	return nil
}

// EntraIssuerURL is the issuer an Entra ID token for the configured tenant
// carries, and the URL its discovery document is found under.
func (c *Config) EntraIssuerURL() string {
	return strings.TrimSuffix(c.EntraAuthorityURL, "/") + "/" + strings.ToLower(c.EntraTenantID) + "/v2.0"
}

func requireAbsoluteHTTPURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q does not parse as a URL: %w", raw, err)
	}
	if (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return fmt.Errorf("%q is not an absolute http or https URL", raw)
	}
	return nil
}
