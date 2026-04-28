// Package authstoreclient is a thin HTTP client for the auth-store service.
// It is the only path llm-bridge-server uses to read or write credentials —
// auth-store is the canonical credential vault per the single-source-of-truth
// directive.
package authstoreclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Credential is the wire-side projection from auth-store. Mirrors the masked
// shape returned by /api/credentials. Resolve() returns full secret material
// in AccessToken / APIKey fields.
type Credential struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	Owner           string `json:"owner,omitempty"`
	Account         string `json:"account,omitempty"`
	Label           string `json:"label,omitempty"`
	IntendedApp     string `json:"intended_app,omitempty"`
	IntendedPurpose string `json:"intended_purpose,omitempty"`
	AuthType        string `json:"auth_type"`
	RefreshMode     string `json:"refresh_mode"`
	APIKey          string `json:"api_key,omitempty"`
	Token           string `json:"token,omitempty"`
	HasRefreshToken bool   `json:"has_refresh_token"`
	ExpiresAt       int64  `json:"expires_at,omitempty"`
	BaseURL         string `json:"base_url,omitempty"`
	Leased          bool   `json:"leased"`
	LeaseHolderApp  string `json:"lease_holder_app,omitempty"`
	LeaseExpiresAt  int64  `json:"lease_expires_at,omitempty"`
	Priority        int    `json:"priority"`
	Enabled         bool   `json:"enabled"`
	LastUsedAt      int64  `json:"last_used_at,omitempty"`
	LastRefreshedAt int64  `json:"last_refreshed_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	LastErrorAt     int64  `json:"last_error_at,omitempty"`
	ErrorCount      int    `json:"error_count,omitempty"`
}

// Resolved is /api/resolve's payload — secrets unmasked.
type Resolved struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Owner       string `json:"owner,omitempty"`
	Account     string `json:"account,omitempty"`
	Label       string `json:"label,omitempty"`
	AuthType    string `json:"auth_type"`
	RefreshMode string `json:"refresh_mode"`
	AccessToken string `json:"access_token,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	Leased      bool   `json:"leased"`
}

// Secret returns the usable secret string regardless of auth_type.
func (r *Resolved) Secret() string {
	if r == nil {
		return ""
	}
	switch r.AuthType {
	case "api_key":
		return r.APIKey
	case "oauth", "token":
		return r.AccessToken
	}
	return ""
}

// Client is an auth-store HTTP client.
type Client struct {
	baseURL string
	token   string
	app     string
	http    *http.Client
}

// New constructs a client. baseURL defaults to AUTH_STORE_URL env var, then
// http://127.0.0.1:8303. token defaults to AUTH_STORE_TOKEN env var.
func New(baseURL, token, app string) *Client {
	if baseURL == "" {
		baseURL = os.Getenv("AUTH_STORE_URL")
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8303"
	}
	if token == "" {
		token = os.Getenv("AUTH_STORE_TOKEN")
	}
	if app == "" {
		app = "llm-bridge-server"
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		app:     app,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path, reason string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if reason != "" {
		req.Header.Set("X-Auth-App", c.app)
		req.Header.Set("X-Auth-Reason", reason)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("auth-store %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("auth-store %s %s: %d %s", method, path, resp.StatusCode, respBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("auth-store decode: %w", err)
		}
	}
	return nil
}

// List returns credentials matching the filter. Secrets are masked.
func (c *Client) List(ctx context.Context, filter ListFilter) ([]Credential, error) {
	q := url.Values{}
	if filter.Provider != "" {
		q.Set("provider", filter.Provider)
	}
	if filter.Owner != "" {
		q.Set("owner", filter.Owner)
	}
	if filter.IntendedApp != "" {
		q.Set("intended_app", filter.IntendedApp)
	}
	path := "/api/credentials"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []Credential
	if err := c.do(ctx, "GET", path, "", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListFilter narrows a List() call.
type ListFilter struct {
	Provider    string
	Owner       string
	IntendedApp string
}

// Get fetches a single credential by id (masked).
func (c *Client) Get(ctx context.Context, id string) (*Credential, error) {
	var out Credential
	if err := c.do(ctx, "GET", "/api/credentials/"+url.PathEscape(id), "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Resolve returns the live access_token / api_key for a credential by id.
func (c *Client) Resolve(ctx context.Context, id, reason string) (*Resolved, error) {
	if reason == "" {
		return nil, errors.New("auth-store: reason is required")
	}
	var out Resolved
	if err := c.do(ctx, "GET", "/api/credentials/"+url.PathEscape(id)+"/resolve", reason, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveByProvider returns the live secret for the best-matching credential
// for (provider, account, intended_app). Account "" means any.
func (c *Client) ResolveByProvider(ctx context.Context, provider, account, intendedApp, reason string) (*Resolved, error) {
	if reason == "" {
		return nil, errors.New("auth-store: reason is required")
	}
	q := url.Values{}
	if account != "" {
		q.Set("account", account)
	}
	if intendedApp != "" {
		q.Set("intended_app", intendedApp)
	}
	path := "/api/resolve/" + url.PathEscape(provider)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out Resolved
	if err := c.do(ctx, "GET", path, reason, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CredentialInput is the body for Create/Update.
type CredentialInput struct {
	Provider     string `json:"provider"`
	Owner        string `json:"owner"`
	Account      string `json:"account,omitempty"`
	Label        string `json:"label,omitempty"`
	IntendedApp  string `json:"intended_app,omitempty"`
	AuthType     string `json:"auth_type"`
	RefreshMode  string `json:"refresh_mode,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	Token        string `json:"token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
}

// Create POSTs a new credential.
func (c *Client) Create(ctx context.Context, in CredentialInput) (*Credential, error) {
	var out Credential
	if err := c.do(ctx, "POST", "/api/credentials", "", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes a credential.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/credentials/"+url.PathEscape(id), "", nil, nil)
}
