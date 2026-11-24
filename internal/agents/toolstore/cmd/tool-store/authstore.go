package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// resolveFromAuthStore returns a credential resolver that calls auth-store's
// HTTP API (canonical credential service). The resolver is invoked from
// /provision when an MCP tool needs an env var resolved.
//
// authStoreURL and token are the auth_store_url and auth_store_token settings
// (AUTH_STORE_URL, AUTH_STORE_TOKEN; the token matches auth-store's own
// AUTHSTORE_TOKEN). The resolver reads no environment variable itself.
//
// Per the single-source-of-truth directive, missing creds fail loudly — no
// env-var or other fallback.
//
// Two fields on auth-store's answer are read by nothing here. `leased` is true
// when a client_lease credential is currently held by another process, and
// auth-store's own doc comment for that response says the token it returns is
// then the last known one and the lease holder is authoritative. `expires_at`
// can be in the past, because auth-store refreshes only server-mode OAuth that
// nobody has leased. Dropping both means a provisioned MCP server can be
// handed a dead token and learn about it as a provider 401 much later, far
// from the cause. Measured against the live :8303 on 2026-08-29: nothing was
// leased and nothing was expired, so this is latent rather than live. What the
// resolver should do instead — refuse, retry, or pass the signal up to
// Provision — is an open question on the noteboard, not settled here.
func resolveFromAuthStore(authStoreURL, token string) func(ctx context.Context, provider string) (string, error) {
	base := strings.TrimRight(authStoreURL, "/")

	client := &http.Client{Timeout: 10 * time.Second}

	return func(ctx context.Context, provider string) (string, error) {
		if provider == "" {
			return "", errors.New("auth-store: empty provider")
		}
		u := fmt.Sprintf("%s/api/resolve/%s", base, url.PathEscape(provider))
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return "", fmt.Errorf("auth-store: build request: %w", err)
		}
		req.Header.Set("X-Auth-App", "tool-store")
		req.Header.Set("X-Auth-Reason", "provision:"+provider)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("auth-store: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("auth-store: provider %q resolve returned %d: %s", provider, resp.StatusCode, body)
		}
		var out struct {
			AuthType    string `json:"auth_type"`
			AccessToken string `json:"access_token"`
			APIKey      string `json:"api_key"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", fmt.Errorf("auth-store: parse: %w", err)
		}
		switch out.AuthType {
		case "api_key":
			if out.APIKey == "" {
				return "", fmt.Errorf("auth-store: provider %q has empty api_key", provider)
			}
			return out.APIKey, nil
		case "oauth", "token":
			if out.AccessToken == "" {
				return "", fmt.Errorf("auth-store: provider %q has empty access_token", provider)
			}
			return out.AccessToken, nil
		default:
			return "", fmt.Errorf("auth-store: provider %q has unsupported auth_type %q", provider, out.AuthType)
		}
	}
}
