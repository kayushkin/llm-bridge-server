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

func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
		cache:   make(map[string]cached),
	}
}

func (c *Client) Token(provider, account string) (string, error) {
	key := provider + "|" + account
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && time.Since(e.fetched) < cacheTTL {
		c.mu.Unlock()
		return e.token, nil
	}
	c.mu.Unlock()

	path := "/api/token/" + url.PathEscape(provider)
	if account != "" && account != "default" {
		path += "/" + url.PathEscape(account)
	}
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("apiauth unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("apiauth %s: %d %s", path, resp.StatusCode, string(body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("apiauth returned empty access_token for %s/%s", provider, account)
	}

	c.mu.Lock()
	c.cache[key] = cached{token: out.AccessToken, fetched: time.Now()}
	c.mu.Unlock()
	return out.AccessToken, nil
}

func (c *Client) Invalidate(provider, account string) {
	c.mu.Lock()
	delete(c.cache, provider+"|"+account)
	c.mu.Unlock()
}
