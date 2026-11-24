package grantstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ResourceChecker asks a resource's owner whether it exists, so a grant never
// names an id nobody hands out.
//
// CheckResourceExists returns nil when the owner has the resource, an error
// wrapping ErrResourceNotFound when the owner answers that it does not, and any
// other error when the owner could not be asked or answered something else. The
// two failures are kept apart because they mean opposite things to a caller: a
// wrong id is theirs to fix, an owner that is down is not.
type ResourceChecker interface {
	CheckResourceExists(ctx context.Context, resourceType, resourceID string) error
}

// What a variable is called that names each owner checked over HTTP, quoted
// in errors so the reader knows which setting to look at. The server this
// store is mounted in decides the values; the names are the ones dash and the
// stores' own units already use.
const (
	skillStoreURLVariable     = "SKILL_STORE_URL"
	toolStoreURLVariable      = "TOOL_STORE_URL"
	referenceStoreURLVariable = "REFERENCE_STORE_URL"
	// referenceStoreServiceTokenVariable is the token reference-store takes
	// from an internal service to look a record up for nobody. Its name is
	// reference-store's own, in the host's shared token file.
	referenceStoreServiceTokenVariable = "REFERENCE_STORE_SERVICE_TOKEN"
	// ReferenceStoreServiceTokenHeader is where reference-store reads it.
	ReferenceStoreServiceTokenHeader = "X-Reference-Store-Service-Token"
)

// ownerCheckTimeout bounds each owner call. A POST waits on it, and the caller is
// a person clicking in a picker: an owner that has not answered in three
// seconds is reported as a 502 rather than left to hang the request.
const ownerCheckTimeout = 3 * time.Second

// ResourceOwners is where each owner checked over HTTP answers, and the
// credential each one that gates its routes is asked with. A token may be
// empty, which sends none.
type ResourceOwners struct {
	SkillStoreURL     string
	ToolStoreURL      string
	ReferenceStoreURL string
	// ReferenceStoreServiceToken is sent to reference-store only. Without it a
	// reference check is a 401 reported as the owner being unable to answer.
	ReferenceStoreServiceToken string
}

// ResourceTypesCheckedOverHTTP are the resource types HTTPResourceChecker
// asks an owner about. Every other type in ResourceTypes is owned by the
// server this store is mounted in (agents, instances, machines, peer
// connections and operation types) or by a store beside it in that server
// (principal-store, kanban-store), and that server checks them in its own
// process.
var ResourceTypesCheckedOverHTTP = []string{ResourceTypeSkill, ResourceTypeTool, ResourceTypeReferenceType, ResourceTypeReference}

// HTTPResourceChecker asks the owners that run as services of their own.
type HTTPResourceChecker struct {
	skillStoreURL              string
	toolStoreURL               string
	referenceStoreURL          string
	referenceStoreServiceToken string
	client                     *http.Client
}

// NewHTTPResourceChecker builds a checker against the given owners.
func NewHTTPResourceChecker(owners ResourceOwners) *HTTPResourceChecker {
	return &HTTPResourceChecker{
		skillStoreURL:              strings.TrimSuffix(owners.SkillStoreURL, "/"),
		toolStoreURL:               strings.TrimSuffix(owners.ToolStoreURL, "/"),
		referenceStoreURL:          strings.TrimSuffix(owners.ReferenceStoreURL, "/"),
		referenceStoreServiceToken: owners.ReferenceStoreServiceToken,
		client:                     &http.Client{Timeout: ownerCheckTimeout},
	}
}

// CheckResourceExists implements ResourceChecker for ResourceTypesCheckedOverHTTP.
// Any other type is an error: its owner is checked in the mounting server's
// own process, and reaching this checker with one is a wiring bug.
func (c *HTTPResourceChecker) CheckResourceExists(ctx context.Context, resourceType, resourceID string) error {
	switch resourceType {
	case ResourceTypeSkill:
		return c.checkByGet(ctx, "skill-store", skillStoreURLVariable,
			c.skillStoreURL+"/skills/"+url.PathEscape(resourceID))
	case ResourceTypeTool:
		return c.checkByGet(ctx, "tool-store", toolStoreURLVariable,
			c.toolStoreURL+"/tools/"+url.PathEscape(resourceID))
	case ResourceTypeReferenceType:
		return c.checkByGet(ctx, "reference-store", referenceStoreURLVariable,
			c.referenceStoreURL+"/types/"+url.PathEscape(resourceID))
	case ResourceTypeReference:
		return c.checkReferenceResolves(ctx, resourceID)
	}
	return fmt.Errorf("the HTTP resource checker asks no owner about resource_type %q: it checks only %s, and the server grant-store is mounted in checks the rest in its own process",
		resourceType, strings.Join(ResourceTypesCheckedOverHTTP, ", "))
}

// get performs one GET and reads the whole body, so every answer below can
// quote the owner's status and body text as it came.
func (c *HTTPResourceChecker) get(ctx context.Context, owner, requestURL string) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	if owner == "reference-store" && c.referenceStoreServiceToken != "" {
		request.Header.Set(ReferenceStoreServiceTokenHeader, c.referenceStoreServiceToken)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("%s did not answer GET %s: %w", owner, requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("%s answered GET %s with %s, but the body could not be read: %w",
			owner, requestURL, response.Status, err)
	}
	return response, body, nil
}

// checkByGet is the check for an owner with a get-by-id route: 2xx exists, 404
// does not, anything else is a failure carrying the owner's status and body.
func (c *HTTPResourceChecker) checkByGet(ctx context.Context, owner, urlVariable, requestURL string) error {
	response, body, err := c.get(ctx, owner, requestURL)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(string(body))
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return nil
	case response.StatusCode == http.StatusNotFound && text == unroutedNotFoundBody:
		// Go's ServeMux answers a path it has no route for with exactly this
		// body. Every owner here answers a missing record with its own text
		// ("instance not found", {"error":"skill not found"}), so this body means
		// the URL points at the wrong service or an owner without the route.
		// Reading it as "does not exist" would refuse every real resource with a
		// 400 telling the caller their id is wrong.
		return fmt.Errorf("%s answered GET %s with %s %q, which is Go's answer for a route that does not exist, not for a missing record: check that %s points at %s",
			owner, requestURL, response.Status, text, urlVariable, owner)
	case response.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s answered GET %s with %s: %s", ErrResourceNotFound, owner, requestURL, response.Status, text)
	default:
		return fmt.Errorf("%s answered GET %s with %s: %s", owner, requestURL, response.Status, text)
	}
}

const unroutedNotFoundBody = "404 page not found"

// checkReferenceResolves asks reference-store, for nobody, what the id names:
// at least one record exists, none does not, and a store that could not say is
// a failure rather than a "does not exist".
func (c *HTTPResourceChecker) checkReferenceResolves(ctx context.Context, resourceID string) error {
	requestURL := c.referenceStoreURL + "/resolve"
	body, err := json.Marshal(map[string][]string{"ids": {resourceID}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("build POST %s: %w", requestURL, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.referenceStoreServiceToken != "" {
		request.Header.Set(ReferenceStoreServiceTokenHeader, c.referenceStoreServiceToken)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("reference-store did not answer POST %s: %w", requestURL, err)
	}
	defer response.Body.Close()
	answerBody, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("reference-store answered POST %s with %s, but the body could not be read: %w", requestURL, response.Status, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("reference-store answered POST %s with %s: %s (check %s and %s)",
			requestURL, response.Status, strings.TrimSpace(string(answerBody)), referenceStoreURLVariable, referenceStoreServiceTokenVariable)
	}
	var answer struct {
		Results map[string][]json.RawMessage `json:"results"`
		Errors  []struct {
			Type  string `json:"type"`
			Error string `json:"error"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(answerBody, &answer); err != nil {
		return fmt.Errorf("reference-store answered POST %s with something that is not its answer: %w", requestURL, err)
	}
	if len(answer.Results[resourceID]) > 0 {
		return nil
	}
	if len(answer.Errors) > 0 {
		return fmt.Errorf("reference-store could not say whether %s exists: %s: %s", resourceID, answer.Errors[0].Type, answer.Errors[0].Error)
	}
	return fmt.Errorf("%w: reference-store finds no record with id %s", ErrResourceNotFound, resourceID)
}
