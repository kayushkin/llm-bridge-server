// Package bundleclient asks bundle-store (:8307) two things about a session
// bundle: does the id exist (at session creation, so a session is never
// created with an id nobody hands out) and what does it resolve to (at spawn,
// so the session is provisioned exactly the tools the bundle names).
//
// bundle-store answers a missing id 404 in its own {"error":…} words and a
// malformed id 400 "invalid id"; both mean the caller's id is wrong. Go's
// plain "404 page not found" means the URL points at a service without the
// route, which is a misconfiguration and reported as one.
package bundleclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is bundle-store answering that it has no such bundle, or that
// the bundle cannot be applied (disabled).
var ErrNotFound = errors.New("bundle not found")

// Client talks to one bundle-store.
type Client struct {
	url  string
	http *http.Client
}

// New builds a client against base (e.g. http://localhost:8307).
func New(base string) *Client {
	return &Client{url: strings.TrimSuffix(base, "/"), http: &http.Client{Timeout: 5 * time.Second}}
}

// Resolution is what a bundle composes to: the tool-store and skill-store ids
// to provision, and the bundles that contributed, for the log line.
type Resolution struct {
	Bundles []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"bundles"`
	Skills []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"skills"`
	Tools []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"tools"`
	// DeniedTools is every tool a bundle in the resolved set denies. A deny
	// anywhere wins, so bundle-store has already taken these out of Tools; a
	// harness tool among them is one the session's harness must not offer.
	DeniedTools []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"denied_tools"`
	// DeniedReadPaths is the paths the session may not read: absolute
	// ("/proc/**") or home-relative ("~/.config/**"), never a harness rule.
	DeniedReadPaths []string `json:"denied_read_paths"`
	Model           string   `json:"model,omitempty"`
	Effort          string   `json:"effort,omitempty"`
}

// DeniedToolIDs is the tool-store ids the resolution denies.
func (r *Resolution) DeniedToolIDs() []int64 {
	ids := make([]int64, 0, len(r.DeniedTools))
	for _, t := range r.DeniedTools {
		ids = append(ids, t.ID)
	}
	return ids
}

// ToolIDs is the tool-store ids the resolution names, in bundle order.
func (r *Resolution) ToolIDs() []int64 {
	ids := make([]int64, 0, len(r.Tools))
	for _, t := range r.Tools {
		ids = append(ids, t.ID)
	}
	return ids
}

// ParseID turns the session's string bundle id into bundle-store's integer
// id. A name ("docker") is refused here, before any call: bundle-store keys
// by id and names are renameable.
func ParseID(bundleID string) (int64, error) {
	id, err := strconv.ParseInt(bundleID, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("bundle_id %q is not a bundle-store id (a positive integer such as \"6\"); a bundle's name is not accepted, because it is renameable", bundleID)
	}
	return id, nil
}

// CheckExists returns nil when bundle-store has the bundle, an error wrapping
// ErrNotFound when it says it does not, and any other error when it could
// not be asked or answered something else.
func (c *Client) CheckExists(ctx context.Context, bundleID string) error {
	if _, err := ParseID(bundleID); err != nil {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	requestURL := c.url + "/bundles/" + url.PathEscape(bundleID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("bundle-store unreachable: GET %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read bundle-store response: %w", err)
	}
	return classify(response, body, "GET "+requestURL)
}

// Resolve composes one bundle: bundle-store's POST /resolve with that id
// alone, which applies the bundle's extends chain plus base. A disabled or
// missing bundle is ErrNotFound — bundle-store refuses rather than composing
// something smaller than what was asked.
func (c *Client) Resolve(ctx context.Context, bundleID string) (*Resolution, error) {
	id, err := ParseID(bundleID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	payload, _ := json.Marshal(map[string]any{"bundle_ids": []int64{id}})
	requestURL := c.url + "/resolve"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build POST %s: %w", requestURL, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("bundle-store unreachable: POST %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read bundle-store response: %w", err)
	}
	if err := classify(response, body, "POST "+requestURL); err != nil {
		return nil, err
	}
	var resolution Resolution
	if err := json.Unmarshal(body, &resolution); err != nil {
		return nil, fmt.Errorf("bundle-store POST %s answered a body that is not a resolution: %w", requestURL, err)
	}
	// A bundle-store too old to send the denials would read as "denies
	// nothing", which starts a session with tools its bundle took away.
	var present map[string]json.RawMessage
	if err := json.Unmarshal(body, &present); err != nil {
		return nil, fmt.Errorf("bundle-store POST %s answered a body that is not an object: %w", requestURL, err)
	}
	for _, field := range []string{"denied_tools", "denied_read_paths"} {
		if _, ok := present[field]; !ok {
			return nil, fmt.Errorf("bundle-store POST %s answered no %s; it predates bundle denials, so bundle %s cannot be applied safely", requestURL, field, bundleID)
		}
	}
	return &resolution, nil
}

const unroutedNotFoundBody = "404 page not found"

func classify(response *http.Response, body []byte, call string) error {
	text := strings.TrimSpace(string(body))
	switch {
	case response.StatusCode/100 == 2:
		return nil
	case response.StatusCode == http.StatusNotFound && text == unroutedNotFoundBody:
		return fmt.Errorf("bundle-store answered %s with %s %q, which is Go's answer for a route that does not exist, not for a missing bundle: check LLMBRIDGE_BUNDLE_STORE_URL", call, response.Status, text)
	case response.StatusCode == http.StatusNotFound, response.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: bundle-store answered %s with %s: %s", ErrNotFound, call, response.Status, text)
	default:
		return fmt.Errorf("bundle-store answered %s with %s: %s", call, response.Status, text)
	}
}
