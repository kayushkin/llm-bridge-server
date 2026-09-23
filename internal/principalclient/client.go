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

// Principal is the part of principal-store's principal record this server
// reads. Kind is "human" or "group"; DisabledAt is 0 for an active principal
// and the epoch second it was disabled otherwise.
//
// IsAdministrator is principal-store's own answer to "may this person do
// anything?", and it is read, never derived: this server does not keep a list
// of administrators and must not infer one from a name, an email or a group.
// principal-store sets it on humans only.
type Principal struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	DisabledAt      int64  `json:"disabled_at"`
	IsAdministrator bool   `json:"is_administrator"`
}

// CheckExists returns nil when principal-store has the principal, an error
// wrapping ErrNotFound when it says it does not, and any other error when it
// could not be asked or answered something else.
func (c *Client) CheckExists(ctx context.Context, principalID string) error {
	_, err := c.Get(ctx, principalID)
	return err
}

// Get returns principal-store's record for principalID, with the same error
// contract as CheckExists. A 2xx whose body is not a principal record with the
// requested id and a kind is an error, not an empty principal.
func (c *Client) Get(ctx context.Context, principalID string) (Principal, error) {
	requestURL := c.url + "/principals/" + url.PathEscape(principalID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return Principal{}, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return Principal{}, fmt.Errorf("principal-store unreachable: GET %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return Principal{}, fmt.Errorf("read principal-store response: %w", err)
	}
	if response.StatusCode/100 == 2 {
		var principal Principal
		if err := json.Unmarshal(body, &principal); err != nil {
			return Principal{}, fmt.Errorf("principal-store answered GET %s with %s and a body that is not a principal: %w", requestURL, response.Status, err)
		}
		if principal.ID != principalID || principal.Kind == "" {
			return Principal{}, fmt.Errorf("principal-store answered GET %s with a record for id %q kind %q, not the principal asked for", requestURL, principal.ID, principal.Kind)
		}
		return principal, nil
	}
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	switch {
	case response.StatusCode == http.StatusNotFound && parsed.Error != "":
		return Principal{}, fmt.Errorf("%w: %s", ErrNotFound, parsed.Error)
	case response.StatusCode == http.StatusNotFound:
		return Principal{}, fmt.Errorf("principal-store answered GET %s with 404 and no JSON error, which is Go's answer for a route that does not exist, not for a missing principal: check LLMBRIDGE_PRINCIPAL_STORE_URL", requestURL)
	default:
		return Principal{}, fmt.Errorf("principal-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
}

// GroupIDsOf returns the ids of the active groups principalID is a direct
// member of. principal-store answers only for a human; asking about any
// other kind is an error from the store, passed on.
func (c *Client) GroupIDsOf(ctx context.Context, principalID string) ([]string, error) {
	requestURL := c.url + "/principals/" + url.PathEscape(principalID) + "/groups"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("principal-store unreachable: GET %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read principal-store response: %w", err)
	}
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("principal-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
	var groups []Principal
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil, fmt.Errorf("principal-store answered GET %s with a body that is not a list of principals: %w", requestURL, err)
	}
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	return ids, nil
}
