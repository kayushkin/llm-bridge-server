package server

// Test stand-ins for the access stores. Before they moved into this process
// principal-store, grant-store and permission-store were reached over HTTP,
// and many tests here were written against small HTTP fakes of them. These
// types answer this server's interfaces (principalDirectory, grantReader,
// permissionEvaluator) from such a fake, decoding its answers the way the
// HTTP clients did, so those tests keep exercising what they were about. The
// in-process stores themselves are tested on Postgres (access_routes_test.go
// and the stores' own tests).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/permclient"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
)

// httpPrincipalDirectoryForTests talks to one principal-store.
type httpPrincipalDirectoryForTests struct {
	url  string
	http *http.Client
}

// testPrincipalDirectoryAt reads the fake principal-store at base.
func testPrincipalDirectoryAt(base string) *httpPrincipalDirectoryForTests {
	return &httpPrincipalDirectoryForTests{url: strings.TrimSuffix(base, "/"), http: &http.Client{Timeout: 5 * time.Second}}
}

// CheckExists returns nil when principal-store has the principal, an error
// wrapping principalclient.ErrNotFound when it says it does not, and any other error when it
// could not be asked or answered something else.
func (c *httpPrincipalDirectoryForTests) CheckExists(ctx context.Context, principalID string) error {
	_, err := c.Get(ctx, principalID)
	return err
}

// Get returns principal-store's record for principalID, with the same error
// contract as CheckExists. A 2xx whose body is not a principal record with the
// requested id and a kind is an error, not an empty principal.
func (c *httpPrincipalDirectoryForTests) Get(ctx context.Context, principalID string) (principalclient.Principal, error) {
	requestURL := c.url + "/principals/" + url.PathEscape(principalID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return principalclient.Principal{}, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return principalclient.Principal{}, fmt.Errorf("principal-store unreachable: GET %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return principalclient.Principal{}, fmt.Errorf("read principal-store response: %w", err)
	}
	if response.StatusCode/100 == 2 {
		var principal principalclient.Principal
		if err := json.Unmarshal(body, &principal); err != nil {
			return principalclient.Principal{}, fmt.Errorf("principal-store answered GET %s with %s and a body that is not a principal: %w", requestURL, response.Status, err)
		}
		if principal.ID != principalID || principal.Kind == "" {
			return principalclient.Principal{}, fmt.Errorf("principal-store answered GET %s with a record for id %q kind %q, not the principal asked for", requestURL, principal.ID, principal.Kind)
		}
		return principal, nil
	}
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	switch {
	case response.StatusCode == http.StatusNotFound && parsed.Error != "":
		return principalclient.Principal{}, fmt.Errorf("%w: %s", principalclient.ErrNotFound, parsed.Error)
	case response.StatusCode == http.StatusNotFound:
		return principalclient.Principal{}, fmt.Errorf("principal-store answered GET %s with 404 and no JSON error, which is Go's answer for a route that does not exist, not for a missing principal: check LLMBRIDGE_PRINCIPAL_STORE_URL", requestURL)
	default:
		return principalclient.Principal{}, fmt.Errorf("principal-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
}

// GroupIDsOf returns the ids of the active groups principalID is a direct
// member of. principal-store answers only for a human; asking about any
// other kind is an error from the store, passed on.
func (c *httpPrincipalDirectoryForTests) GroupIDsOf(ctx context.Context, principalID string) ([]string, error) {
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

// ResolveIdentityBinding asks principal-store which principal an outside
// identity is bound to. It returns an error wrapping principalclient.ErrIdentityNotProvisioned
// when principal-store answers 404 with its own error body, and any other
// error when it could not be asked or answered something else. A 2xx whose
// binding does not name the identity asked about is an error, not a match.
func (c *httpPrincipalDirectoryForTests) ResolveIdentityBinding(ctx context.Context, provider, tenantID, subject string) (principalclient.IdentityResolution, error) {
	query := url.Values{"provider": {provider}, "tenant_id": {tenantID}, "subject": {subject}}
	requestURL := c.url + "/identity-bindings/resolve?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return principalclient.IdentityResolution{}, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return principalclient.IdentityResolution{}, fmt.Errorf("principal-store unreachable: GET %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return principalclient.IdentityResolution{}, fmt.Errorf("read principal-store response: %w", err)
	}
	if response.StatusCode/100 == 2 {
		var resolution principalclient.IdentityResolution
		if err := json.Unmarshal(body, &resolution); err != nil {
			return principalclient.IdentityResolution{}, fmt.Errorf("principal-store answered GET %s with %s and a body that is not an identity resolution: %w", requestURL, response.Status, err)
		}
		binding := resolution.Binding
		if binding.ID == "" || binding.Provider != provider || !strings.EqualFold(binding.TenantID, tenantID) || !strings.EqualFold(binding.Subject, subject) ||
			binding.UnlinkedAt != 0 || resolution.Principal.ID == "" || resolution.Principal.ID != binding.PrincipalID || resolution.Principal.Kind == "" {
			return principalclient.IdentityResolution{}, fmt.Errorf("principal-store answered GET %s with binding %+v and principal %q, which is not an active binding of the identity asked about", requestURL, binding, resolution.Principal.ID)
		}
		return resolution, nil
	}
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	switch {
	case response.StatusCode == http.StatusNotFound && parsed.Error != "":
		return principalclient.IdentityResolution{}, fmt.Errorf("%w: %s", principalclient.ErrIdentityNotProvisioned, parsed.Error)
	case response.StatusCode == http.StatusNotFound:
		return principalclient.IdentityResolution{}, fmt.Errorf("principal-store answered GET %s with 404 and no JSON error, which is Go's answer for a route that does not exist, not for an unprovisioned identity: check LLMBRIDGE_PRINCIPAL_STORE_URL", requestURL)
	default:
		return principalclient.IdentityResolution{}, fmt.Errorf("principal-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
}

// Create makes a principal of the given kind and returns its id.
func (c *httpPrincipalDirectoryForTests) Create(ctx context.Context, kind, displayName string) (string, error) {
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
func (c *httpPrincipalDirectoryForTests) SetAdministrator(ctx context.Context, principalID string, isAdministrator bool) error {
	return c.write(ctx, http.MethodPatch, "/principals/"+url.PathEscape(principalID), map[string]bool{"is_administrator": isAdministrator}, nil)
}

// AddMember puts a principal in a group. Already a member is not an error.
func (c *httpPrincipalDirectoryForTests) AddMember(ctx context.Context, groupID, memberID string) error {
	return c.write(ctx, http.MethodPut, "/principals/"+url.PathEscape(groupID)+"/members/"+url.PathEscape(memberID), nil, nil)
}

// RemoveMember takes a principal out of a group.
func (c *httpPrincipalDirectoryForTests) RemoveMember(ctx context.Context, groupID, memberID string) error {
	return c.write(ctx, http.MethodDelete, "/principals/"+url.PathEscape(groupID)+"/members/"+url.PathEscape(memberID), nil, nil)
}

// write sends one request and decodes a 2xx answer into answer when it is not
// nil. Any other status is an error carrying principal-store's own body.
func (c *httpPrincipalDirectoryForTests) write(ctx context.Context, method, path string, body any, answer any) error {
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

// httpGrantReaderForTests talks to one grant-store.
type httpGrantReaderForTests struct {
	url  string
	http *http.Client
}

// testGrantReaderAt reads the fake grant-store at base.
func testGrantReaderAt(base string) *httpGrantReaderForTests {
	return &httpGrantReaderForTests{url: strings.TrimSuffix(base, "/"), http: &http.Client{Timeout: 5 * time.Second}}
}

// grantRowForTests is the part of a grant-store row this client reads.
type grantRowForTests struct {
	ID           string `json:"id"`
	PrincipalID  string `json:"principal_id"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

// EffectiveResourceIDs returns the resource ids the principal's effective
// grants of one relation name, for one resource type. An empty answer means
// the principal — and every group they are in — holds no such grant; it is
// not an error.
func (c *httpGrantReaderForTests) EffectiveResourceIDs(ctx context.Context, principalID, relation, resourceType string) ([]string, error) {
	grants, err := c.effective(ctx, principalID, relation, resourceType)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(grants))
	for _, g := range grants {
		ids = append(ids, g.ResourceID)
	}
	return ids, nil
}

func (c *httpGrantReaderForTests) effective(ctx context.Context, principalID, relation, resourceType string) ([]grantRowForTests, error) {
	requestURL := fmt.Sprintf("%s/principals/%s/effective?relation=%s&resource_type=%s",
		c.url, url.PathEscape(principalID), url.QueryEscape(relation), url.QueryEscape(resourceType))
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
		return nil, fmt.Errorf("%w: grant-store answered GET %s with 404: %s", grantclient.ErrPrincipalUnknown, requestURL, strings.TrimSpace(string(body)))
	case response.StatusCode/100 != 2:
		return nil, fmt.Errorf("grant-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
	var grants []grantRowForTests
	if err := json.Unmarshal(body, &grants); err != nil {
		return nil, fmt.Errorf("grant-store answered GET %s with a body that is not a list of grants: %w", requestURL, err)
	}
	return grants, nil
}

// Create records a grant with the service token and returns its id. grant-store
// is idempotent on the active tuple, so a grant that already exists answers
// with its id too.
func (c *httpGrantReaderForTests) Create(ctx context.Context, input grantclient.GrantInput) (string, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	requestURL := c.url + "/grants"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, strings.NewReader(string(data)))
	if err != nil {
		return "", fmt.Errorf("build POST %s: %w", requestURL, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("grant-store unreachable: POST %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read grant-store response: %w", err)
	}
	if response.StatusCode/100 != 2 {
		return "", fmt.Errorf("grant-store answered POST %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
	var created grantRowForTests
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("grant-store answered POST %s with a body that is not a grant: %s", requestURL, strings.TrimSpace(string(body)))
	}
	return created.ID, nil
}

// httpPermissionEvaluatorForTests is a permission-store /evaluate caller. Safe for concurrent use.
type httpPermissionEvaluatorForTests struct {
	url  string
	http *http.Client
}

// testPermissionEvaluatorAt asks the fake permission-store at baseURL (e.g.
// "http://localhost:8304"); the /evaluate suffix is appended internally.
func testPermissionEvaluatorAt(baseURL string) *httpPermissionEvaluatorForTests {
	return &httpPermissionEvaluatorForTests{
		url: baseURL,
		// Loopback HTTP — typical evaluate is sub-millisecond. The timeout
		// is a generous ceiling so a wedged store reaches the caller's
		// error-handling path rather than holding the parent request
		// forever.
		http: &http.Client{Timeout: 3 * time.Second},
	}
}

// Evaluate posts the request and parses the response. Any transport or
// parse failure is returned as an error, never a verdict: no rule has been
// consulted, so there is nothing to report but the failure. The prehook
// turns that into an "ask" for a session with a human attached and a deny
// for one without (writeHookNoVerdict). Neither path silently allows.
func (c *httpPermissionEvaluatorForTests) Evaluate(ctx context.Context, req permclient.Request) (*permclient.Result, error) {
	body, err := json.Marshal(map[string]any{"bridge_id": req.BridgeSessionID, "instance_id": req.InstanceID, "tool": req.Tool, "input": req.Input})
	if err != nil {
		return nil, fmt.Errorf("marshal evaluate request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build evaluate request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("permission-store unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read evaluate response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("permission-store HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed permclient.Result
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode evaluate response: %w", err)
	}
	switch parsed.Outcome {
	case "allow", "deny", "ask":
		return &parsed, nil
	}
	return nil, fmt.Errorf("permission-store returned unknown outcome %q", parsed.Outcome)
}

// useFakePrincipalStore points server's principal reads, and the identity
// binding routes it passes through to principal-store, at the HTTP fake at
// base. Only the pass-through handler is set on the mounted routes, so a test
// that calls /principal-store/ itself uses the real stores instead.
func useFakePrincipalStore(server *Server, base string) {
	server.principalClient = testPrincipalDirectoryAt(base)
	server.access.Store(&accessRouteHandlers{principalStoreRoot: reverseProxyTo(base)})
}

// reverseProxyTo forwards every request to base, as principal-store's routes
// at their own root.
func reverseProxyTo(base string) http.Handler {
	target, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	return httputil.NewSingleHostReverseProxy(target)
}
