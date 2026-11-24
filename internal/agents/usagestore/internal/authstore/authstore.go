// Package authstore is a minimal client for resolving credentials from
// auth-store (the canonical credential vault on :8303). usage-store needs to
// pull admin API keys to call provider cost endpoints — this is the smallest
// surface that lets it do that without dragging in the full auth-store SDK.
package authstore

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// escapePathSegment makes a value safe to interpolate into ONE path segment of
// a URL. The credential ids this client is handed come from auth-store's own
// /api/credentials answer and are not checked on this side, so a "/", "?" or
// "#" in one would otherwise change which endpoint gets addressed — with this
// client's bearer token and audit headers attached to the wrong request.
//
// url.PathEscape, not url.QueryEscape: the value lands in a path, where a space
// is "%20" and QueryEscape's "+" would be a literal plus. Escaping is chosen by
// where the value lands, not by what the value is. Every sink in this repo is a
// URL — it imports os/exec nowhere — so PathEscape leaving "&" and "$" intact
// (both are legal path sub-delims, RFC 3986 s3.3) is correct here.
func escapePathSegment(s string) string { return url.PathEscape(s) }

// Client resolves credentials by ID. All calls require BearerToken plus the
// X-Auth-App / X-Auth-Reason audit headers — auth-store rejects key-touching
// routes without them.
type Client struct {
	BaseURL     string
	BearerToken string
	App         string // identifies the caller in auth-store's audit log
	HTTPClient  *http.Client
}

// New returns a client with sane defaults. App should be the name of the
// service making the call ("usage-store").
func New(baseURL, token, app string) *Client {
	return &Client{
		BaseURL:     baseURL,
		BearerToken: token,
		App:         app,
		HTTPClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Resolved is the subset of auth-store's resolvedView we care about. We only
// need the API key and label (for logging).
type Resolved struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
	AuthType string `json:"auth_type"`
	APIKey   string `json:"api_key"`
}

// CredentialSummary is the subset of /api/credentials we care about. The list
// endpoint redacts secrets — to read api_key, you have to call ResolveByID.
type CredentialSummary struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
	AuthType string `json:"auth_type"`
}

// ListCredentials returns every credential auth-store knows about (no
// secrets). Used for admin-key discovery: scan provider=foo + auth_type=api_key
// and resolve each to find ones whose api_key looks like an admin key.
func (c *Client) ListCredentials() ([]CredentialSummary, error) {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/credentials", nil)
	if err != nil {
		return nil, err
	}
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth-store: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("auth-store list returned %d: %s", resp.StatusCode, string(body))
	}
	// auth-store's handleListCredentials writes []credentialView directly, so
	// the body is a bare JSON array. There is no {"credentials": [...]}
	// variant to tolerate — a decode failure here means the route changed
	// shape and should be loud rather than silently empty.
	var arr []CredentialSummary
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, fmt.Errorf("parse list: %w", err)
	}
	return arr, nil
}

// ResolveByID fetches a credential and returns its API key. Reason is logged
// in auth-store's audit trail.
func (c *Client) ResolveByID(id, reason string) (*Resolved, error) {
	u := fmt.Sprintf("%s/api/credentials/%s/resolve", c.BaseURL, escapePathSegment(id))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}
	req.Header.Set("X-Auth-App", c.App)
	req.Header.Set("X-Auth-Reason", reason)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth-store: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("auth-store returned %d resolving %s: %s", resp.StatusCode, id, string(body))
	}

	var out Resolved
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse auth-store response: %w", err)
	}
	if out.APIKey == "" {
		return nil, fmt.Errorf("credential %s has no api_key (auth_type=%s)", id, out.AuthType)
	}
	return &out, nil
}
