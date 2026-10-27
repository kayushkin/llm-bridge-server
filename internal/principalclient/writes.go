package principalclient

// Writes this server makes to principal-store, for one purpose: granting
// another llm-bridge-server access (internal/server/peers.go). This server
// makes the peer principal it grants to, sets its administrator flag, and puts
// it in or takes it out of the level's group. It writes nothing else there.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// KindGroup is principal-store's kind for a group, which a level's
// provisioning makes. Spelled here because creating one must name it.
const KindGroup = "group"

// Create makes a principal of the given kind and returns its id.
func (c *Client) Create(ctx context.Context, kind, displayName string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := c.write(ctx, http.MethodPost, "/principals", map[string]string{"kind": kind, "display_name": displayName}, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("principal-store answered POST /principals with no id")
	}
	return created.ID, nil
}

// SetAdministrator sets principal-store's is_administrator on a principal.
func (c *Client) SetAdministrator(ctx context.Context, principalID string, isAdministrator bool) error {
	return c.write(ctx, http.MethodPatch, "/principals/"+url.PathEscape(principalID), map[string]bool{"is_administrator": isAdministrator}, nil)
}

// AddMember puts a principal in a group. Already a member is not an error.
func (c *Client) AddMember(ctx context.Context, groupID, memberID string) error {
	return c.write(ctx, http.MethodPut, "/principals/"+url.PathEscape(groupID)+"/members/"+url.PathEscape(memberID), nil, nil)
}

// RemoveMember takes a principal out of a group.
func (c *Client) RemoveMember(ctx context.Context, groupID, memberID string) error {
	return c.write(ctx, http.MethodDelete, "/principals/"+url.PathEscape(groupID)+"/members/"+url.PathEscape(memberID), nil, nil)
}

// write sends one request and decodes a 2xx answer into answer when it is not
// nil. Any other status is an error carrying principal-store's own body.
func (c *Client) write(ctx context.Context, method, path string, body any, answer any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.url+path, reader)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("principal-store unreachable: %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read principal-store's answer to %s %s: %w", method, path, err)
	}
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("principal-store answered %s %s with %s: %s", method, path, response.Status, strings.TrimSpace(string(data)))
	}
	if answer != nil {
		if err := json.Unmarshal(data, answer); err != nil {
			return fmt.Errorf("principal-store answered %s %s with a body this server cannot read: %w", method, path, err)
		}
	}
	return nil
}
