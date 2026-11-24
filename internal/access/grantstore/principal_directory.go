package grantstore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// PrincipalDirectory is principal-store as this store needs it: does a
// principal exist, what kind is it, and which active groups is it in. Both
// writes (is the grantee real?) and the effective-set read (which groups'
// grants does this human inherit?) go through it.
//
// LookupPrincipal returns an error wrapping ErrPrincipalNotFound when
// principal-store answers 404, and any other error when it could not be asked
// or answered something else. The two are kept apart because they mean
// opposite things to a caller: a wrong id is theirs to fix, a directory that is
// down is not.
type PrincipalDirectory interface {
	LookupPrincipal(ctx context.Context, principalID string) (*PrincipalSummary, error)
}

// PrincipalSummary is the part of a principal-store row this store reads.
type PrincipalSummary struct {
	ID         string
	Kind       string // as principal-store spells it; shown in messages, never compared
	DisabledAt int64
	// ActsInThisDeployment and ActsAsCaller are principal-store's answers for
	// this principal's kind: may it hold grants, and may a request act as it.
	// This store reads them rather than comparing Kind to a word, so a kind
	// principal-store adds (a peer, say) is handled by what principal-store
	// says it may do.
	ActsInThisDeployment bool
	ActsAsCaller         bool
	// IsAdministrator is principal-store's one fact about a person that this
	// store acts on: an administrator may read and write every grant, whatever
	// it is on and whoever holds it.
	IsAdministrator bool
	// GroupIDs is the active groups a human belongs to, as principal-store's
	// GET /principals/{id} expands them. Empty for a group: groups do not nest.
	GroupIDs []string
}

// PRINCIPAL_STORE_URL is the name dash already uses for where principals are
// checked; settings.go declares it with DefaultPrincipalStoreURL.
const (
	DefaultPrincipalStoreURL  = "http://127.0.0.1:8314"
	principalStoreURLVariable = "PRINCIPAL_STORE_URL"
	principalStoreOwnerName   = "principal-store"
)

// HTTPPrincipalDirectory reads the real principal-store over HTTP.
type HTTPPrincipalDirectory struct {
	baseURL string
	client  *http.Client
}

// NewHTTPPrincipalDirectory builds a directory against principal-store's base URL.
func NewHTTPPrincipalDirectory(baseURL string) *HTTPPrincipalDirectory {
	return &HTTPPrincipalDirectory{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		client:  &http.Client{Timeout: ownerCheckTimeout},
	}
}

// LookupPrincipal implements PrincipalDirectory with one GET. principal-store
// answers a disabled principal like any other (a departed human still has to
// render), and expands a human's active groups on the same call, so existence,
// kind and inheritance come from a single round trip.
func (d *HTTPPrincipalDirectory) LookupPrincipal(ctx context.Context, principalID string) (*PrincipalSummary, error) {
	requestURL := d.baseURL + "/principals/" + url.PathEscape(principalID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s did not answer GET %s: %w", principalStoreOwnerName, requestURL, err)
	}
	defer response.Body.Close()
	var body struct {
		ID              string `json:"id"`
		Kind            string `json:"kind"`
		DisabledAt      int64  `json:"disabled_at"`
		IsAdministrator bool   `json:"is_administrator"`
		// Pointers, so a principal-store that predates these answers is an
		// error naming that, not a principal that silently may do nothing.
		ActsInThisDeployment *bool `json:"acts_in_this_deployment"`
		ActsAsCaller         *bool `json:"acts_as_caller"`
		Groups               []struct {
			ID string `json:"id"`
		} `json:"groups"`
		Error string `json:"error"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&body)
	switch {
	case response.StatusCode == http.StatusNotFound && body.Error != "":
		return nil, fmt.Errorf("%w: %s answered GET %s with %s: %s",
			ErrPrincipalNotFound, principalStoreOwnerName, requestURL, response.Status, body.Error)
	case response.StatusCode == http.StatusNotFound:
		// Go's ServeMux answers a path it has no route for with a plain-text
		// 404 that does not decode as JSON. That means the URL points at the
		// wrong service, not that the principal is missing.
		return nil, fmt.Errorf("%s answered GET %s with %s and no JSON error, which is Go's answer for a route that does not exist, not for a missing principal: check that %s points at %s",
			principalStoreOwnerName, requestURL, response.Status, principalStoreURLVariable, principalStoreOwnerName)
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, fmt.Errorf("%s answered GET %s with %s: %s", principalStoreOwnerName, requestURL, response.Status, body.Error)
	case decodeErr != nil:
		return nil, fmt.Errorf("%s answered GET %s with a body that is not a principal: %w", principalStoreOwnerName, requestURL, decodeErr)
	case body.ID != principalID:
		return nil, fmt.Errorf("%s answered GET %s with principal %q, not %q", principalStoreOwnerName, requestURL, body.ID, principalID)
	case body.ActsInThisDeployment == nil || body.ActsAsCaller == nil:
		return nil, fmt.Errorf("%s answered GET %s without acts_in_this_deployment and acts_as_caller, so it is older than this store needs: deploy principal-store first", principalStoreOwnerName, requestURL)
	}
	summary := &PrincipalSummary{ID: body.ID, Kind: body.Kind, DisabledAt: body.DisabledAt, IsAdministrator: body.IsAdministrator,
		ActsInThisDeployment: *body.ActsInThisDeployment, ActsAsCaller: *body.ActsAsCaller}
	for _, group := range body.Groups {
		summary.GroupIDs = append(summary.GroupIDs, group.ID)
	}
	return summary, nil
}
