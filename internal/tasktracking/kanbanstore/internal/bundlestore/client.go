// Package bundlestore is kanban-store's client for bundle-store, the registry
// of session bundles (named sets of skills and tools). It exists for one
// check: before a board's default_bundle_id is stored, the id must be one
// bundle-store hands out. Same reasoning as internal/llmbridge — a wrong id
// here fails every later dispatch far from the PATCH that caused it.
package bundlestore

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
)

// Client talks to one bundle-store.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client with the 3-second timeout the board-settings write path
// budgets for each existence check.
func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), HTTP: &http.Client{Timeout: 3 * time.Second}}
}

// ErrNotFound is returned when bundle-store has no bundle with the id, or
// refuses the id's shape (bundle ids are integers; "docker" is a name).
var ErrNotFound = errors.New("not found in bundle-store")

// CheckBundleExists asks GET /bundles/{id}. bundle-store answers a missing
// bundle 404 and a malformed id 400 "invalid id"; both mean the caller's id is
// wrong. Any other non-200 is returned naming the status and body.
func (c *Client) CheckBundleExists(id string) error {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/bundles/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	// The id is the caller's, so every message below names it bounded: the
	// path as shown, and the URL inside a transport error.
	shownPath := "/bundles/" + url.PathEscape(boundedtext.Value(id))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) {
			urlError.URL = c.BaseURL + shownPath
		}
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := strings.TrimSpace(string(body))
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusNotFound && text == unroutedNotFoundBody:
		return fmt.Errorf("bundle-store GET %s answered %s %q, which is Go's answer for a route that does not exist, not for a missing record: check that BUNDLE_STORE_URL points at bundle-store", shownPath, resp.Status, text)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: bundle %s", ErrNotFound, boundedtext.Value(id))
	case resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: bundle-store refused id %q (%s) — send bundle-store's numeric id, not the bundle's name", ErrNotFound, boundedtext.Value(id), text)
	default:
		return fmt.Errorf("bundle-store GET %s: %s — %s", shownPath, resp.Status, text)
	}
}

const unroutedNotFoundBody = "404 page not found"
