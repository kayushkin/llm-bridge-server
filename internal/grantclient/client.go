// Package grantclient reads grant-store (:8315): which tools a principal's
// effective grants say a session started as them may be offered.
//
// grant-store's effective set is the principal's own active grants plus
// every active group's, with the groups read live from principal-store on
// each call — so a membership change is visible at the next spawn without
// this server being told. The store answers 404 for a principal it does not
// know and 502 when principal-store is down, never an empty list; this
// client keeps those apart so the spawn can tell "nothing granted" from
// "could not find out".
package grantclient

import (
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

// ErrPrincipalUnknown is grant-store answering that principal-store has no
// such principal. A session carrying that id was created against a directory
// that has since lost the row, or never had it.
var ErrPrincipalUnknown = errors.New("principal unknown")

// Client talks to one grant-store.
type Client struct {
	url  string
	http *http.Client
}

// New builds a client against base (e.g. http://127.0.0.1:8315).
func New(base string) *Client {
	return &Client{url: strings.TrimSuffix(base, "/"), http: &http.Client{Timeout: 5 * time.Second}}
}

// grant is the part of a grant-store row this client reads.
type grant struct {
	ID           string `json:"id"`
	PrincipalID  string `json:"principal_id"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

// EffectiveToolIDs returns the tool-store ids the principal's effective
// can_use grants name. An empty answer means the principal — and every group
// they are in — holds no such grant; it is not an error.
func (c *Client) EffectiveToolIDs(ctx context.Context, principalID string) ([]int64, error) {
	requestURL := fmt.Sprintf("%s/principals/%s/effective?relation=can_use&resource_type=tool",
		c.url, url.PathEscape(principalID))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("grant-store unreachable: GET %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read grant-store response: %w", err)
	}
	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: grant-store answered GET %s with 404: %s", ErrPrincipalUnknown, requestURL, strings.TrimSpace(string(body)))
	case response.StatusCode/100 != 2:
		return nil, fmt.Errorf("grant-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
	var grants []grant
	if err := json.Unmarshal(body, &grants); err != nil {
		return nil, fmt.Errorf("grant-store answered GET %s with a body that is not a list of grants: %w", requestURL, err)
	}
	ids := make([]int64, 0, len(grants))
	for _, g := range grants {
		id, err := strconv.ParseInt(g.ResourceID, 10, 64)
		if err != nil {
			// tool-store ids are integers and grant-store checks the shape on
			// write, so this is a store answering something it never accepts.
			return nil, fmt.Errorf("grant %s names tool resource_id %q, which is not a tool-store id", g.ID, g.ResourceID)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
