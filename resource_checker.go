package grantstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

// Where each owner listens when the environment says nothing, and the variable
// that says otherwise; settings.go declares each pair as one setting. The
// shipped unit sets them explicitly; the defaults exist so a bare run on this
// host works.
// LLM_BRIDGE_URL is the name llm-bridge-adapter and dash already read, and
// SKILL_STORE_URL and TOOL_STORE_URL are dash's, so one host has one name per
// owner.
const (
	DefaultLLMBridgeServerURL = "http://127.0.0.1:8160"
	DefaultSkillStoreURL      = "http://127.0.0.1:8301"
	DefaultToolStoreURL       = "http://127.0.0.1:8302"
	DefaultKanbanStoreURL     = "http://127.0.0.1:8305"
	DefaultReferenceStoreURL  = "http://127.0.0.1:8343"

	llmBridgeServerURLVariable = "LLM_BRIDGE_URL"
	skillStoreURLVariable      = "SKILL_STORE_URL"
	toolStoreURLVariable       = "TOOL_STORE_URL"
	kanbanStoreURLVariable     = "KANBAN_STORE_URL"
	referenceStoreURLVariable  = "REFERENCE_STORE_URL"
	// referenceStoreServiceTokenVariable is the token reference-store takes
	// from an internal service to look a record up for nobody. Its name is
	// reference-store's own, in the host's shared token file.
	referenceStoreServiceTokenVariable = "REFERENCE_STORE_SERVICE_TOKEN"
	// ReferenceStoreServiceTokenHeader is where reference-store reads it.
	ReferenceStoreServiceTokenHeader = "X-Reference-Store-Service-Token"
	// kanbanStoreServiceTokenVariable holds the token kanban-store accepts from
	// internal services when it enforces principals. Without it a board check
	// against an enforcing kanban-store is a 401, reported as the owner being
	// unable to answer — never as the board not existing.
	kanbanStoreServiceTokenVariable = "KANBAN_STORE_SERVICE_TOKEN"
	// KanbanStoreServiceTokenHeader is the header kanban-store reads that token
	// from. Declared in kanban-store's internal/api/principal_access.go too;
	// the two must match.
	KanbanStoreServiceTokenHeader = "X-Kanban-Store-Service-Token"
	// llmBridgeServiceTokenVariable holds the token llm-bridge-server takes
	// from an internal service, shared with every service that calls it.
	llmBridgeServiceTokenVariable = "LLMBRIDGE_SERVICE_TOKEN"
	// LLMBridgeServiceTokenHeader is where llm-bridge-server reads it.
	LLMBridgeServiceTokenHeader = "X-LLM-Bridge-Service-Token"
)

// ownerCheckTimeout bounds each owner call. A POST waits on it, and the caller is
// a person clicking in a picker: an owner that has not answered in three
// seconds is reported as a 502 rather than left to hang the request.
const ownerCheckTimeout = 3 * time.Second

// ResourceOwners is where each owner answers, and the credential each one
// that gates its routes is asked with. A token may be empty, which sends none.
type ResourceOwners struct {
	LLMBridgeServerURL string
	// LLMBridgeServiceToken is sent to llm-bridge-server only. Its instance,
	// machine and peer connection routes are operator routes, so without it
	// every such check is a 401 reported as the owner being unable to answer.
	LLMBridgeServiceToken string
	PrincipalStoreURL     string
	SkillStoreURL         string
	ToolStoreURL          string
	KanbanStoreURL        string
	// KanbanStoreServiceToken is sent to kanban-store only.
	KanbanStoreServiceToken string
	ReferenceStoreURL       string
	// ReferenceStoreServiceToken is sent to reference-store only. Without it a
	// reference check is a 401 reported as the owner being unable to answer.
	ReferenceStoreServiceToken string
}

// HTTPResourceChecker asks the real owners over HTTP.
type HTTPResourceChecker struct {
	llmBridgeServerURL    string
	llmBridgeServiceToken string
	principalStoreURL     string
	skillStoreURL         string
	toolStoreURL          string
	kanbanStoreURL        string
	// kanbanStoreServiceToken is sent to kanban-store only, and only when set.
	kanbanStoreServiceToken    string
	referenceStoreURL          string
	referenceStoreServiceToken string
	client                     *http.Client
}

// NewHTTPResourceChecker builds a checker against the given owners.
func NewHTTPResourceChecker(owners ResourceOwners) *HTTPResourceChecker {
	return &HTTPResourceChecker{
		llmBridgeServerURL:         strings.TrimSuffix(owners.LLMBridgeServerURL, "/"),
		llmBridgeServiceToken:      owners.LLMBridgeServiceToken,
		principalStoreURL:          strings.TrimSuffix(owners.PrincipalStoreURL, "/"),
		skillStoreURL:              strings.TrimSuffix(owners.SkillStoreURL, "/"),
		toolStoreURL:               strings.TrimSuffix(owners.ToolStoreURL, "/"),
		kanbanStoreURL:             strings.TrimSuffix(owners.KanbanStoreURL, "/"),
		kanbanStoreServiceToken:    owners.KanbanStoreServiceToken,
		referenceStoreURL:          strings.TrimSuffix(owners.ReferenceStoreURL, "/"),
		referenceStoreServiceToken: owners.ReferenceStoreServiceToken,
		client:                     &http.Client{Timeout: ownerCheckTimeout},
	}
}

// CheckResourceExists implements ResourceChecker.
func (c *HTTPResourceChecker) CheckResourceExists(ctx context.Context, resourceType, resourceID string) error {
	switch resourceType {
	case ResourceTypeAgent:
		return c.checkAgentInList(ctx, resourceID)
	case ResourceTypeInstance:
		return c.checkByGet(ctx, "llm-bridge-server", llmBridgeServerURLVariable,
			c.llmBridgeServerURL+"/instances/"+url.PathEscape(resourceID))
	case ResourceTypeMachine:
		return c.checkByGet(ctx, "llm-bridge-server", llmBridgeServerURLVariable,
			c.llmBridgeServerURL+"/machines/"+url.PathEscape(resourceID))
	case ResourceTypeSkill:
		return c.checkByGet(ctx, "skill-store", skillStoreURLVariable,
			c.skillStoreURL+"/skills/"+url.PathEscape(resourceID))
	case ResourceTypeTool:
		return c.checkByGet(ctx, "tool-store", toolStoreURLVariable,
			c.toolStoreURL+"/tools/"+url.PathEscape(resourceID))
	case ResourceTypeBoard:
		return c.checkByGet(ctx, "kanban-store", kanbanStoreURLVariable,
			c.kanbanStoreURL+"/api/boards/"+url.PathEscape(resourceID))
	case ResourceTypeOperationType:
		return c.checkOperationTypeInList(ctx, resourceID)
	case ResourceTypePrincipal:
		return c.checkByGet(ctx, principalStoreOwnerName, principalStoreURLVariable,
			c.principalStoreURL+"/principals/"+url.PathEscape(resourceID))
	case ResourceTypePeerConnection:
		return c.checkByGet(ctx, "llm-bridge-server", llmBridgeServerURLVariable,
			c.llmBridgeServerURL+"/peer-connections/"+url.PathEscape(resourceID))
	case ResourceTypeReferenceType:
		return c.checkByGet(ctx, "reference-store", referenceStoreURLVariable,
			c.referenceStoreURL+"/types/"+url.PathEscape(resourceID))
	case ResourceTypeReference:
		return c.checkReferenceResolves(ctx, resourceID)
	}
	// Unreachable through the store, which validates the type first. Reaching it
	// means a type was added to resource_type.go without an owner check here.
	return fmt.Errorf("no owner check for resource_type %q: resource_type.go names %s, and each needs a case in CheckResourceExists",
		resourceType, strings.Join(ResourceTypes, ", "))
}

// get performs one GET and reads the whole body, so every answer below can
// quote the owner's status and body text as it came.
func (c *HTTPResourceChecker) get(ctx context.Context, owner, requestURL string) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	if owner == "kanban-store" && c.kanbanStoreServiceToken != "" {
		request.Header.Set(KanbanStoreServiceTokenHeader, c.kanbanStoreServiceToken)
	}
	if owner == "reference-store" && c.referenceStoreServiceToken != "" {
		request.Header.Set(ReferenceStoreServiceTokenHeader, c.referenceStoreServiceToken)
	}
	if owner == "llm-bridge-server" && c.llmBridgeServiceToken != "" {
		request.Header.Set(LLMBridgeServiceTokenHeader, c.llmBridgeServiceToken)
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

// checkAgentInList scans GET /agents for a numeric id. agent-store's
// single-agent route is keyed by slug, and the slug is renameable — a name, not
// an id — so the list is the only place the id can be looked up.
func (c *HTTPResourceChecker) checkAgentInList(ctx context.Context, resourceID string) error {
	const owner = "llm-bridge-server"
	requestURL := c.llmBridgeServerURL + "/agents"
	response, body, err := c.get(ctx, owner, requestURL)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s answered GET %s with %s: %s", owner, requestURL, response.Status, strings.TrimSpace(string(body)))
	}
	var agents []struct {
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &agents); err != nil {
		return fmt.Errorf("%s answered GET %s with a body that is not a JSON array of agents with integer ids: %w",
			owner, requestURL, err)
	}
	for index, agent := range agents {
		if agent.ID == nil {
			return fmt.Errorf("%s answered GET %s with an agent at index %d that has no id", owner, requestURL, index)
		}
		if strconv.FormatInt(*agent.ID, 10) == resourceID {
			return nil
		}
	}
	return fmt.Errorf("%w: %s answered GET %s with %d agents and none has id %s",
		ErrResourceNotFound, owner, requestURL, len(agents), resourceID)
}

// checkOperationTypeInList looks for the type in llm-bridge-server's
// GET /operation-types, which the bridge serves without a credential.
func (c *HTTPResourceChecker) checkOperationTypeInList(ctx context.Context, resourceID string) error {
	const owner = "llm-bridge-server"
	requestURL := c.llmBridgeServerURL + "/operation-types"
	response, body, err := c.get(ctx, owner, requestURL)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s answered GET %s with %s: %s", owner, requestURL, response.Status, strings.TrimSpace(string(body)))
	}
	var operationTypes []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &operationTypes); err != nil {
		return fmt.Errorf("%s answered GET %s with a body that is not a JSON array of operation types: %w", owner, requestURL, err)
	}
	known := make([]string, 0, len(operationTypes))
	for _, operationType := range operationTypes {
		if operationType.Type == resourceID {
			return nil
		}
		known = append(known, operationType.Type)
	}
	return fmt.Errorf("%w: %s runs no operation type %q; it runs %s", ErrResourceNotFound, owner, resourceID, strings.Join(known, ", "))
}

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
