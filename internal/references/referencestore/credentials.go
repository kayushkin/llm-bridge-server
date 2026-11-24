package referencestore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Credentials presents each type's credential to its owning service.
type Credentials struct {
	BridgeServiceToken string
	AuthStoreURL       string
	AuthStoreToken     string
	// AuthStoreProviders maps a service to its auth-store provider.
	AuthStoreProviders map[string]string
	HTTPClient         *http.Client
	// TTL is how long a key read from auth-store is reused.
	TTL time.Duration

	mu   sync.Mutex
	keys map[string]cachedKey
}

type cachedKey struct {
	key       string
	expiresAt time.Time
}

// Apply sets the credential on a request to row's service.
func (c *Credentials) Apply(ctx context.Context, request *http.Request, row *ReferenceType) error {
	switch row.Credential {
	case CredentialNone:
		return nil
	case CredentialBridgeServiceToken:
		if c.BridgeServiceToken == "" {
			return fmt.Errorf("type %s needs bridge_service_token, which is not set", row.Type)
		}
		request.Header.Set("X-LLM-Bridge-Service-Token", c.BridgeServiceToken)
		return nil
	case CredentialAuthStore:
		key, err := c.authStoreKey(ctx, row.Service)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+key)
		return nil
	}
	return fmt.Errorf("type %s has credential %q, which reference-store does not know", row.Type, row.Credential)
}

func (c *Credentials) authStoreKey(ctx context.Context, service string) (string, error) {
	provider := c.AuthStoreProviders[service]
	if provider == "" {
		return "", fmt.Errorf("auth_store_providers names no provider for %s", service)
	}
	if c.AuthStoreURL == "" {
		return "", fmt.Errorf("auth_store_url is not set, and %s's credential is in auth-store", service)
	}
	now := time.Now()
	c.mu.Lock()
	if cached, ok := c.keys[provider]; ok && now.Before(cached.expiresAt) {
		c.mu.Unlock()
		return cached.key, nil
	}
	c.mu.Unlock()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.AuthStoreURL, "/")+"/api/resolve/"+provider, nil)
	if err != nil {
		return "", err
	}
	if c.AuthStoreToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.AuthStoreToken)
	}
	request.Header.Set("X-Auth-App", ServiceName)
	request.Header.Set("X-Auth-Reason", "reference lookup in "+service)
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("auth-store: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth-store: provider %s answered %d", provider, response.StatusCode)
	}
	var credential struct {
		AuthType string `json:"auth_type"`
		APIKey   string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &credential); err != nil {
		return "", fmt.Errorf("auth-store: provider %s: %w", provider, err)
	}
	if credential.AuthType != "api_key" || credential.APIKey == "" {
		return "", fmt.Errorf("auth-store: provider %s is auth_type %q with no api_key", provider, credential.AuthType)
	}
	c.mu.Lock()
	if c.keys == nil {
		c.keys = map[string]cachedKey{}
	}
	c.keys[provider] = cachedKey{key: credential.APIKey, expiresAt: now.Add(c.TTL)}
	c.mu.Unlock()
	return credential.APIKey, nil
}
