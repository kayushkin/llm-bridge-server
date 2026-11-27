// Package apiauth keeps its name for compatibility with existing imports, but
// now talks to auth-store on :8303 instead of the legacy apiauth service.
// auth-store is the canonical credential vault.
package apiauth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	token   string
	fetched time.Time
}

const cacheTTL = 55 * time.Second

// New constructs a token-source client. baseURL should be auth-store's API
// root (e.g. http://127.0.0.1:8303). token is the AUTHSTORE_TOKEN bearer.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
		cache:   make(map[string]cached),
	}
}

// Token returns a valid access token for (provider, account). It calls
// auth-store's /api/resolve/{provider}?account=... endpoint, which will
// transparently refresh expired server-mode OAuth tokens before returning.
func (c *Client) Token(provider, account string) (string, error) {
	key := provider + "|" + account
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && time.Since(e.fetched) < cacheTTL {
		c.mu.Unlock()
		return e.token, nil
	}
	c.mu.Unlock()

	u, err := url.Parse(c.baseURL + "/api/resolve/" + url.PathEscape(provider))
	if err != nil {
		return "", fmt.Errorf("auth-store: build url: %w", err)
	}
	q := u.Query()
	if account != "" && account != "default" {
		q.Set("account", account)
	} else if account == "default" {
		// auth-store stores apiauth tokens with account="default"; pass it
		// through so the row matches.
		q.Set("account", "default")
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Auth-App", "mailstack")
	req.Header.Set("X-Auth-Reason", "mail:"+provider+":"+account)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth-store unavailable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth-store /resolve/%s?account=%s: %d %s", provider, account, resp.StatusCode, string(body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("auth-store returned empty access_token for %s/%s", provider, account)
	}

	c.mu.Lock()
	c.cache[key] = cached{token: out.AccessToken, fetched: time.Now()}
	c.mu.Unlock()
	return out.AccessToken, nil
}

// Invalidate drops the cached token for (provider, account), forcing the next
// Token() call to re-fetch from auth-store.
func (c *Client) Invalidate(provider, account string) {
	c.mu.Lock()
	delete(c.cache, provider+"|"+account)
	c.mu.Unlock()
}
