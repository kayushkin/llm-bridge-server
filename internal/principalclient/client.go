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

// KindPeer is principal-store's kind for another llm-bridge-server linked to
// this one. Spelled here because a peer invite may be made only for one: what
// a peer may do is read from the answers below, never from this word.
const KindPeer = "peer"

// Principal is the part of principal-store's principal record this server
// reads. DisabledAt is 0 for an active principal and the epoch second it was
// disabled otherwise.
//
// IsAdministrator is principal-store's own answer to "may this person do
// anything?", and it is read, never derived: this server does not keep a list
// of administrators and must not infer one from a name, an email or a group.
//
// ActsAsCaller and RestrictedToItsGrants are principal-store's answers for the
// principal's kind: may a request act as it (a human or a peer), and does
// holding no grant of a relation refuse it (a peer) rather than leave it
// unrestricted (a human). This server reads them instead of comparing Kind to
// a word.
type Principal struct {
	ID                    string `json:"id"`
	Kind                  string `json:"kind"`
	DisabledAt            int64  `json:"disabled_at"`
	IsAdministrator       bool   `json:"is_administrator"`
	ActsAsCaller          bool   `json:"acts_as_caller"`
	RestrictedToItsGrants bool   `json:"restricted_to_its_grants"`
}

// UnmarshalJSON refuses a record without acts_as_caller or
// restricted_to_its_grants: a principal-store older than those answers would
// otherwise read as a principal that may do nothing and is never restricted,
// with nothing saying why.
func (principal *Principal) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID                    string `json:"id"`
		Kind                  string `json:"kind"`
		DisabledAt            int64  `json:"disabled_at"`
		IsAdministrator       bool   `json:"is_administrator"`
		ActsAsCaller          *bool  `json:"acts_as_caller"`
		RestrictedToItsGrants *bool  `json:"restricted_to_its_grants"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.ActsAsCaller == nil || wire.RestrictedToItsGrants == nil {
		return fmt.Errorf("principal %q has no acts_as_caller or restricted_to_its_grants, so principal-store is older than this server needs: deploy principal-store first", wire.ID)
	}
	*principal = Principal{ID: wire.ID, Kind: wire.Kind, DisabledAt: wire.DisabledAt, IsAdministrator: wire.IsAdministrator,
		ActsAsCaller: *wire.ActsAsCaller, RestrictedToItsGrants: *wire.RestrictedToItsGrants}
	return nil
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
	// Only the ids are read, so a group's own record is not held to
	// Principal's checks.
	var groups []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil, fmt.Errorf("principal-store answered GET %s with a body that is not a list of principals: %w", requestURL, err)
	}
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	return ids, nil
}

// ErrIdentityNotProvisioned is principal-store answering that no active
// binding links the identity to a principal: the person has not been
// provisioned, and may not sign in.
var ErrIdentityNotProvisioned = errors.New("identity is not bound to any principal")

// IdentityBinding is principal-store's record linking an outside identity —
// for Entra ID the tenant id and object id — to one human principal.
type IdentityBinding struct {
	ID           string `json:"id"`
	Seq          int64  `json:"seq"`
	PrincipalID  string `json:"principal_id"`
	Provider     string `json:"provider"`
	TenantID     string `json:"tenant_id"`
	Subject      string `json:"subject"`
	LinkedAt     int64  `json:"linked_at"`
	LinkedBy     string `json:"linked_by"`
	LinkNote     string `json:"link_note"`
	UnlinkedAt   int64  `json:"unlinked_at"`
	UnlinkedBy   string `json:"unlinked_by"`
	UnlinkReason string `json:"unlink_reason"`
}

// IdentityResolution is principal-store's answer to "who does this identity
// sign in as": the active binding and the principal it names, which comes back
// even when disabled so the caller can say why it refuses.
type IdentityResolution struct {
	Binding   IdentityBinding `json:"binding"`
	Principal Principal       `json:"principal"`
}

// ResolveIdentityBinding asks principal-store which principal an outside
// identity is bound to. It returns an error wrapping ErrIdentityNotProvisioned
// when principal-store answers 404 with its own error body, and any other
// error when it could not be asked or answered something else. A 2xx whose
// binding does not name the identity asked about is an error, not a match.
func (c *Client) ResolveIdentityBinding(ctx context.Context, provider, tenantID, subject string) (IdentityResolution, error) {
	query := url.Values{"provider": {provider}, "tenant_id": {tenantID}, "subject": {subject}}
	requestURL := c.url + "/identity-bindings/resolve?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return IdentityResolution{}, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return IdentityResolution{}, fmt.Errorf("principal-store unreachable: GET %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return IdentityResolution{}, fmt.Errorf("read principal-store response: %w", err)
	}
	if response.StatusCode/100 == 2 {
		var resolution IdentityResolution
		if err := json.Unmarshal(body, &resolution); err != nil {
			return IdentityResolution{}, fmt.Errorf("principal-store answered GET %s with %s and a body that is not an identity resolution: %w", requestURL, response.Status, err)
		}
		binding := resolution.Binding
		if binding.ID == "" || binding.Provider != provider || !strings.EqualFold(binding.TenantID, tenantID) || !strings.EqualFold(binding.Subject, subject) ||
			binding.UnlinkedAt != 0 || resolution.Principal.ID == "" || resolution.Principal.ID != binding.PrincipalID || resolution.Principal.Kind == "" {
			return IdentityResolution{}, fmt.Errorf("principal-store answered GET %s with binding %+v and principal %q, which is not an active binding of the identity asked about", requestURL, binding, resolution.Principal.ID)
		}
		return resolution, nil
	}
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	switch {
	case response.StatusCode == http.StatusNotFound && parsed.Error != "":
		return IdentityResolution{}, fmt.Errorf("%w: %s", ErrIdentityNotProvisioned, parsed.Error)
	case response.StatusCode == http.StatusNotFound:
		return IdentityResolution{}, fmt.Errorf("principal-store answered GET %s with 404 and no JSON error, which is Go's answer for a route that does not exist, not for an unprovisioned identity: check LLMBRIDGE_PRINCIPAL_STORE_URL", requestURL)
	default:
		return IdentityResolution{}, fmt.Errorf("principal-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
}

// BaseURL is principal-store's base URL, for the administrator routes that
// pass a request through to it unchanged.
func (c *Client) BaseURL() string {
	return c.url
}
