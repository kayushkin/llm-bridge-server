// Package grantstore is kanban-store's client for grant-store, the registry of
// who may use what. kanban-store reads it only when it runs with principal
// enforcement on (see internal/api/principal_access.go): once per request, to
// learn which boards the calling principal holds can_view, can_edit or
// can_administer on, and once per board a principal creates, to give the
// creator can_administer on it.
//
// Group membership is expanded by grant-store, never here: GET
// /principals/{id}/effective already answers with the principal's own grants
// and every grant held by a group it belongs to.
package grantstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The board relations as grant-store spells them. grant-store serves the
// vocabulary at GET /relations; these three are the ones this store enforces,
// and a grant-store that stopped serving them would refuse the grants below
// with a 400 naming the ones it does serve.
const (
	RelationCanView       = "can_view"
	RelationCanEdit       = "can_edit"
	RelationCanAdminister = "can_administer"
	ResourceTypeBoard     = "board"
)

// ServiceTokenHeader is where grant-store reads its service token.
const ServiceTokenHeader = "X-Grant-Store-Service-Token"

// Client talks to one grant-store.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// ServiceToken is sent on every call when set. kanban-store reads grants as
	// itself, not as the user, and a grant-store that enforces principals
	// answers 401 without it — which surfaces as a 502 here, never as no grants.
	ServiceToken string
}

// New returns a client with a 3-second timeout, the same budget kanban-store
// gives its other owner checks. serviceToken may be empty for a grant-store
// that does not enforce principals.
func New(baseURL, serviceToken string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), HTTP: &http.Client{Timeout: 3 * time.Second}, ServiceToken: serviceToken}
}

func (c *Client) send(request *http.Request) (*http.Response, error) {
	if c.ServiceToken != "" {
		request.Header.Set(ServiceTokenHeader, c.ServiceToken)
	}
	return c.HTTP.Do(request)
}

// ErrPrincipalNotFound is grant-store answering 404 for the principal: the id
// is not one principal-store knows.
var ErrPrincipalNotFound = errors.New("principal not found by grant-store")

// BoardGrant is one active grant on a board, own or inherited from a group.
type BoardGrant struct {
	PrincipalID string `json:"principal_id"`
	Relation    string `json:"relation"`
	BoardID     string `json:"resource_id"`
}

// EffectiveBoardGrants returns every active board grant the principal holds,
// directly or through a group.
func (c *Client) EffectiveBoardGrants(principalID string) ([]BoardGrant, error) {
	requestURL := c.BaseURL + "/principals/" + url.PathEscape(principalID) + "/effective?resource_type=" + ResourceTypeBoard
	request, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.send(request)
	if err != nil {
		return nil, fmt.Errorf("grant-store did not answer GET %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("grant-store answered GET %s with %s, but the body could not be read: %w", requestURL, response.Status, err)
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrPrincipalNotFound, strings.TrimSpace(string(body)))
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grant-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
	var grants []BoardGrant
	if err := json.Unmarshal(body, &grants); err != nil {
		return nil, fmt.Errorf("grant-store answered GET %s with a body that is not a JSON array of grants: %w", requestURL, err)
	}
	return grants, nil
}

// GrantBoardAdministration gives a principal can_administer on a board. It is
// called right after the principal creates the board, so the board's creator
// is not locked out of it. grant-store is idempotent on the active tuple, so
// 201 and 200 are both success.
func (c *Client) GrantBoardAdministration(principalID, boardID string) error {
	payload, err := json.Marshal(map[string]string{
		"principal_id":  principalID,
		"relation":      RelationCanAdminister,
		"resource_type": ResourceTypeBoard,
		"resource_id":   boardID,
		"note":          "created the board",
	})
	if err != nil {
		return err
	}
	requestURL := c.BaseURL + "/grants"
	request, err := http.NewRequest(http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.send(request)
	if err != nil {
		return fmt.Errorf("grant-store did not answer POST %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return fmt.Errorf("grant-store answered POST %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
