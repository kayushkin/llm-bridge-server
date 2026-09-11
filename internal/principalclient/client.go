// Package principalclient asks principal-store (:8314) whether a principal
// exists, so a session is never created as an id nobody hands out.
//
// principal-store answers a disabled principal like any other — a departed
// human's grants are dormant, not invalid — so "exists" here includes
// disabled. A 404 with principal-store's own {"error":…} body means the id is
// unknown; Go's plain "404 page not found" means the URL points at a service
// without the route, which is a misconfiguration and reported as one.
package principalclient

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

// ErrNotFound is principal-store answering that it has no such principal.
var ErrNotFound = errors.New("principal not found")

// Client talks to one principal-store.
type Client struct {
	url  string
	http *http.Client
}

// New builds a client against base (e.g. http://127.0.0.1:8314).
func New(base string) *Client {
	return &Client{url: strings.TrimSuffix(base, "/"), http: &http.Client{Timeout: 5 * time.Second}}
}

// CheckExists returns nil when principal-store has the principal, an error
// wrapping ErrNotFound when it says it does not, and any other error when it
// could not be asked or answered something else.
func (c *Client) CheckExists(ctx context.Context, principalID string) error {
	requestURL := c.url + "/principals/" + url.PathEscape(principalID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("principal-store unreachable: GET %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read principal-store response: %w", err)
	}
	if response.StatusCode/100 == 2 {
		return nil
	}
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	switch {
	case response.StatusCode == http.StatusNotFound && parsed.Error != "":
		return fmt.Errorf("%w: %s", ErrNotFound, parsed.Error)
	case response.StatusCode == http.StatusNotFound:
		return fmt.Errorf("principal-store answered GET %s with 404 and no JSON error, which is Go's answer for a route that does not exist, not for a missing principal: check LLMBRIDGE_PRINCIPAL_STORE_URL", requestURL)
	default:
		return fmt.Errorf("principal-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
}
