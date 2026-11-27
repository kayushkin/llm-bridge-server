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

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/permissions"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
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

// Get returns principal-store's record for principalID, an error wrapping
// principals.ErrNotFound when it says it has none, and any other error when it
// could not be asked or answered something else. A 2xx whose body is not a principal record with the
// requested id and a kind is an error, not an empty principal.
func (c *httpPrincipalDirectoryForTests) Get(ctx context.Context, principalID string, _ bool) (*principals.Principal, error) {
	requestURL := c.url + "/principals/" + url.PathEscape(principalID)
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
	if response.StatusCode/100 == 2 {
		var principal principals.Principal
		if err := json.Unmarshal(body, &principal); err != nil {
			return nil, fmt.Errorf("principal-store answered GET %s with %s and a body that is not a principal: %w", requestURL, response.Status, err)
		}
		if principal.ID != principalID || principal.Kind == "" {
			return nil, fmt.Errorf("principal-store answered GET %s with a record for id %q kind %q, not the principal asked for", requestURL, principal.ID, principal.Kind)
		}
		return &principal, nil
	}
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	switch {
	case response.StatusCode == http.StatusNotFound && parsed.Error != "":
		return nil, fmt.Errorf("%w: %s", principals.ErrNotFound, parsed.Error)
	case response.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("principal-store answered GET %s with 404 and no JSON error, which is Go's answer for a route that does not exist, not for a missing principal: check LLMBRIDGE_PRINCIPAL_STORE_URL", requestURL)
	default:
		return nil, fmt.Errorf("principal-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
}

// ListGroups returns the active groups principalID is a direct member of,
// each with only its id. principal-store answers only for a human; asking
// about any other kind is an error from the store, passed on.
func (c *httpPrincipalDirectoryForTests) ListGroups(ctx context.Context, principalID string, _ bool) ([]*principals.Principal, error) {
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
	listed := make([]*principals.Principal, 0, len(groups))
	for _, group := range groups {
		listed = append(listed, &principals.Principal{ID: group.ID, Kind: principals.KindGroup})
	}
	return listed, nil
}

// ResolveIdentity asks principal-store which principal an outside identity is
// bound to. It returns an error wrapping principals.ErrNotFound
// when principal-store answers 404 with its own error body, and any other
// error when it could not be asked or answered something else. A 2xx whose
// binding does not name the identity asked about is an error, not a match.
func (c *httpPrincipalDirectoryForTests) ResolveIdentity(ctx context.Context, provider, tenantID, subject string) (*principals.IdentityBindingResolution, error) {
	query := url.Values{"provider": {provider}, "tenant_id": {tenantID}, "subject": {subject}}
	requestURL := c.url + "/identity-bindings/resolve?" + query.Encode()
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
	if response.StatusCode/100 == 2 {
		var resolution principals.IdentityBindingResolution
		if err := json.Unmarshal(body, &resolution); err != nil || resolution.Binding == nil || resolution.Principal == nil {
			return nil, fmt.Errorf("principal-store answered GET %s with %s and a body that is not an identity resolution: %w", requestURL, response.Status, err)
		}
		binding := resolution.Binding
		if binding.ID == "" || binding.Provider != provider || !strings.EqualFold(binding.TenantID, tenantID) || !strings.EqualFold(binding.Subject, subject) ||
			binding.UnlinkedAt != 0 || resolution.Principal.ID == "" || resolution.Principal.ID != binding.PrincipalID || resolution.Principal.Kind == "" {
			return nil, fmt.Errorf("principal-store answered GET %s with binding %+v and principal %q, which is not an active binding of the identity asked about", requestURL, binding, resolution.Principal.ID)
		}
		return &resolution, nil
	}
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	switch {
	case response.StatusCode == http.StatusNotFound && parsed.Error != "":
		return nil, fmt.Errorf("%w: %s", principals.ErrNotFound, parsed.Error)
	case response.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("principal-store answered GET %s with 404 and no JSON error, which is Go's answer for a route that does not exist, not for an unprovisioned identity: check LLMBRIDGE_PRINCIPAL_STORE_URL", requestURL)
	default:
		return nil, fmt.Errorf("principal-store answered GET %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
}

// Create makes a principal of the input's kind and display name.
func (c *httpPrincipalDirectoryForTests) Create(ctx context.Context, input *principals.Principal) (*principals.Principal, error) {
	var created principals.Principal
	if err := c.write(ctx, http.MethodPost, "/principals", map[string]string{"kind": input.Kind, "display_name": input.DisplayName}, &created); err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, fmt.Errorf("principal-store answered POST /principals with no id")
	}
	return &created, nil
}

// Patch sets principal-store's is_administrator on a principal, the one field
// this server patches.
func (c *httpPrincipalDirectoryForTests) Patch(ctx context.Context, principalID string, patch principals.Patch) (*principals.Principal, error) {
	if patch.IsAdministrator == nil {
		return nil, fmt.Errorf("the fake principal-store patches only is_administrator")
	}
	return &principals.Principal{ID: principalID}, c.write(ctx, http.MethodPatch, "/principals/"+url.PathEscape(principalID), map[string]bool{"is_administrator": *patch.IsAdministrator}, nil)
}

// AddMember puts a principal in a group. Already a member is not an error.
func (c *httpPrincipalDirectoryForTests) AddMember(ctx context.Context, groupID, memberID string) (bool, error) {
	return true, c.write(ctx, http.MethodPut, "/principals/"+url.PathEscape(groupID)+"/members/"+url.PathEscape(memberID), nil, nil)
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

// Effective answers the principal's effective grants of one relation on one
// resource type, as grant-store's GET /principals/{id}/effective does. The
// directory is grant-store's own business, so the fake does not ask it.
func (c *httpGrantReaderForTests) Effective(ctx context.Context, _ grants.PrincipalDirectory, principalID, relation, resourceType string) ([]*grants.Grant, error) {
	rows, err := c.effective(ctx, principalID, relation, resourceType)
	if err != nil {
		return nil, err
	}
	effective := make([]*grants.Grant, 0, len(rows))
	for _, row := range rows {
		effective = append(effective, &grants.Grant{ID: row.ID, PrincipalID: row.PrincipalID, Relation: relation, ResourceType: row.ResourceType, ResourceID: row.ResourceID})
	}
	return effective, nil
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
		return nil, fmt.Errorf("%w: grant-store answered GET %s with 404: %s", grants.ErrNotFound, requestURL, strings.TrimSpace(string(body)))
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
func (c *httpGrantReaderForTests) Create(ctx context.Context, _ grants.PrincipalDirectory, _ grants.ResourceChecker, input grants.GrantRequest) (*grants.Grant, bool, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return nil, false, err
	}
	requestURL := c.url + "/grants"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, strings.NewReader(string(data)))
	if err != nil {
		return nil, false, fmt.Errorf("build POST %s: %w", requestURL, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("grant-store unreachable: POST %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, false, fmt.Errorf("read grant-store response: %w", err)
	}
	if response.StatusCode/100 != 2 {
		return nil, false, fmt.Errorf("grant-store answered POST %s with %s: %s", requestURL, response.Status, strings.TrimSpace(string(body)))
	}
	var created grantRowForTests
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return nil, false, fmt.Errorf("grant-store answered POST %s with a body that is not a grant: %s", requestURL, strings.TrimSpace(string(body)))
	}
	return &grants.Grant{ID: created.ID, PrincipalID: created.PrincipalID, ResourceType: created.ResourceType, ResourceID: created.ResourceID}, true, nil
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
func (c *httpPermissionEvaluatorForTests) Evaluate(ctx context.Context, req permissions.EvaluateRequest) (permissions.EvaluateResponse, error) {
	body, err := json.Marshal(map[string]any{"bridge_id": req.BridgeID, "instance_id": req.InstanceID, "tool": req.Tool, "input": req.Input})
	if err != nil {
		return permissions.EvaluateResponse{}, fmt.Errorf("marshal evaluate request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/evaluate", bytes.NewReader(body))
	if err != nil {
		return permissions.EvaluateResponse{}, fmt.Errorf("build evaluate request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return permissions.EvaluateResponse{}, fmt.Errorf("permission-store unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return permissions.EvaluateResponse{}, fmt.Errorf("read evaluate response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return permissions.EvaluateResponse{}, fmt.Errorf("permission-store HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed permissions.EvaluateResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return permissions.EvaluateResponse{}, fmt.Errorf("decode evaluate response: %w", err)
	}
	switch parsed.Outcome {
	case "allow", "deny", "ask":
		return parsed, nil
	}
	return permissions.EvaluateResponse{}, fmt.Errorf("permission-store returned unknown outcome %q", parsed.Outcome)
}

// useFakePrincipalStore points server's principal reads, and the identity
// binding routes it passes through to principal-store, at the HTTP fake at
// base. Only the pass-through handler is set on the mounted routes, so a test
// that calls /principals/ itself uses the real stores instead.
func useFakePrincipalStore(server *Server, base string) {
	server.principalStore = testPrincipalDirectoryAt(base)
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
