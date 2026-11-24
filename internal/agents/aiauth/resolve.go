package aiauth

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// providerEnvVars maps provider names to their environment variable names.
var providerEnvVars = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
	"google":    "GOOGLE_API_KEY",
	"cohere":    "COHERE_API_KEY",
}

// RegisterProviderEnvVar registers an env var name for a provider.
func RegisterProviderEnvVar(provider, envVar string) {
	providerEnvVars[provider] = envVar
}

// providerRegistry holds registered providers for token refresh.
var providerRegistry = map[string]Provider{}

// RegisterProvider registers a provider for use in token refresh.
func RegisterProvider(p Provider) {
	providerRegistry[p.ID()] = p
}

// ResolveKey returns a valid API key for the given provider.
// Priority: env var → oauth (auto-refresh if expired) → token → api_key
func (s *Store) ResolveKey(provider string) (string, error) {
	// 1. Check env var
	if envName, ok := providerEnvVars[provider]; ok {
		if val := os.Getenv(envName); val != "" {
			return val, nil
		}
	}

	// 2. Get profiles ordered by priority
	creds := s.ProfilesForProvider(provider)
	if len(creds) == 0 {
		return "", fmt.Errorf("no credentials found for provider %q", provider)
	}

	now := time.Now().UnixMilli()

	for _, c := range creds {
		switch c.Type {
		case "oauth":
			key := c.Access
			if key == "" {
				continue
			}
			// Check expiry and refresh if needed
			if c.Expires > 0 && c.Expires < now {
				if p, ok := providerRegistry[provider]; ok {
					refreshed, err := p.RefreshToken(c)
					if err != nil {
						continue // try next credential
					}
					name := s.FindProfileName(c)
					_ = s.UpdateProfile(name, refreshed)
					key = refreshed.Access

					// Sync to <provider>:manual for OpenClaw compatibility
					manualName := provider + ":manual"
					if _, exists := s.data.Profiles[manualName]; exists {
						manualCred := &Credential{
							Type:     "token",
							Provider: provider,
							Token:    refreshed.Access,
							Expires:  refreshed.Expires,
							Email:    refreshed.Email,
						}
						_ = s.UpdateProfile(manualName, manualCred)
					}
				} else {
					continue // expired and no provider to refresh
				}
			}
			return key, nil

		case "token":
			if c.Token == "" {
				continue
			}
			if c.Expires > 0 && c.Expires < now {
				continue // expired
			}
			return c.Token, nil

		case "api_key":
			if c.Key == "" {
				continue
			}
			return c.Key, nil
		}
	}

	return "", fmt.Errorf("no valid credentials for provider %q", provider)
}

// AnthropicKey is a convenience for ResolveKey("anthropic").
func (s *Store) AnthropicKey() (string, error) {
	return s.ResolveKey("anthropic")
}

// MaskKey masks a key for display, showing only the first and last 4 chars.
//
// Both cuts land on a rune boundary. The masked value goes to `aiauth status`
// on stdout, so a split rune shows as a replacement glyph rather than
// corrupting a stored or transmitted value — tier 3 by consequence. Every
// provider key format on this box is ASCII, so this is unswept rather than on
// fire. The value is display-only, so shortening a budget by up to three bytes
// is the right repair rather than a refusal.
func MaskKey(key string) string {
	if len(key) <= 12 {
		return strings.Repeat("*", len(key))
	}
	return truncateAtRuneBoundary(key, 4) + "..." + suffixAtRuneBoundary(key, 4)
}
