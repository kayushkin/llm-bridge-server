// Package multichat is kanban-store's client for the one multichat route it
// uses: POST /api/messages/send, which messages one person — identified by
// their bridge puppet id — in the direct-message room shared with them.
package multichat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSource returns multichat's API token for one call.
type TokenSource func(ctx context.Context) (string, error)

type Client struct {
	baseURL    string
	token      TokenSource
	httpClient *http.Client
}

func New(baseURL string, token TokenSource) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// SendResult is multichat's answer: where the message went.
type SendResult struct {
	RoomID      string `json:"room_id"`
	EventID     string `json:"event_id"`
	CreatedRoom bool   `json:"created_room"`
}

// SendDirectMessage sends body to recipientUserID. Any non-2xx answer is an
// error carrying multichat's status and body.
func (c *Client) SendDirectMessage(ctx context.Context, recipientUserID, body string) (SendResult, error) {
	token, err := c.token(ctx)
	if err != nil {
		return SendResult{}, err
	}
	payload, err := json.Marshal(map[string]string{"user_id": recipientUserID, "body": body})
	if err != nil {
		return SendResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/messages/send", bytes.NewReader(payload))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SendResult{}, fmt.Errorf("multichat: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SendResult{}, fmt.Errorf("multichat POST /api/messages/send: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var result SendResult
	if err := json.Unmarshal(data, &result); err != nil {
		return SendResult{}, fmt.Errorf("multichat POST /api/messages/send: decode %q: %w", data, err)
	}
	return result, nil
}

// authStoreProvider is the auth-store provider holding multichat's token.
const authStoreProvider = "multichat"

// AuthStoreTokenSource resolves multichat's token from auth-store on every
// call, so a rotated token is picked up without a restart and kanban-store
// never holds a copy.
func AuthStoreTokenSource(authStoreURL, authStoreToken string) TokenSource {
	base := strings.TrimRight(authStoreURL, "/")
	httpClient := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context) (string, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", base+"/api/resolve/"+url.PathEscape(authStoreProvider), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+authStoreToken)
		req.Header.Set("X-Auth-App", "kanban-store")
		req.Header.Set("X-Auth-Reason", "message-trigger")
		resp, err := httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("auth-store: %w", err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("auth-store: resolve %s: %d %s", authStoreProvider, resp.StatusCode, strings.TrimSpace(string(data)))
		}
		var out struct {
			AuthType string `json:"auth_type"`
			APIKey   string `json:"api_key"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("auth-store: resolve %s: %w", authStoreProvider, err)
		}
		if out.AuthType != "api_key" || out.APIKey == "" {
			return "", fmt.Errorf("auth-store: provider %s is auth_type %q with an empty api_key; expected an api_key credential", authStoreProvider, out.AuthType)
		}
		return out.APIKey, nil
	}
}
