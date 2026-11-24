package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/agents/bundlestore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/toolstore"
	"github.com/kayushkin/llm-bridge-server/internal/bundleclient"
)

// fakeToolRegistry stands in for the tool-store module in tests that pin how
// the server treats its answers and its failures. Rows are written as the JSON
// tool-store answers with, so a test reads as the store's own records.
type fakeToolRegistry struct {
	t *testing.T
	// harnessTools are the kind=harness rows ListTools answers, filtered by
	// harness; nil uses harnessToolRowsForTests.
	harnessTools []map[string]any
	// instanceTools are each instance's opt-ins. A missing instance has none.
	instanceTools map[string][]map[string]any
	// listInstanceToolsError, listToolsError, provisionError and
	// recordObservedError make that call fail, as an unreachable store did.
	listInstanceToolsError error
	listToolsError         error
	provisionError         error
	recordObservedError    error
	// provisionAnswer is the MCP config Provision answers a request with, as
	// JSON; nil answers one server per requested id and one for an instance.
	provisionAnswer func(request toolstore.ProvisionRequest) string
	// provisionBodies records each Provision request as the JSON body the
	// standalone store's POST /provision received, with only the fields set.
	provisionBodies []map[string]any
	// observed records each harness tool report; created is what it answers.
	observed []toolstore.ObservedHarnessToolsRequest
	created  []string
}

func newFakeToolRegistry(t *testing.T) *fakeToolRegistry {
	t.Helper()
	return &fakeToolRegistry{t: t}
}

// errFakeToolStoreDown is what every call of a registry made with
// unreachableToolRegistry answers.
var errFakeToolStoreDown = errors.New("fake tool-store: connection refused")

// unreachableToolRegistry fails every call, as a tool-store that is down did.
func unreachableToolRegistry(t *testing.T) *fakeToolRegistry {
	registry := newFakeToolRegistry(t)
	registry.listInstanceToolsError = errFakeToolStoreDown
	registry.listToolsError = errFakeToolStoreDown
	registry.provisionError = errFakeToolStoreDown
	registry.recordObservedError = errFakeToolStoreDown
	return registry
}

func (registry *fakeToolRegistry) toolsFromRows(rows []map[string]any) []toolstore.Tool {
	registry.t.Helper()
	encoded, err := json.Marshal(rows)
	if err != nil {
		registry.t.Fatalf("fake tool-store rows: %v", err)
	}
	tools := []toolstore.Tool{}
	if err := json.Unmarshal(encoded, &tools); err != nil {
		registry.t.Fatalf("fake tool-store rows are not tools: %v", err)
	}
	return tools
}

func (registry *fakeToolRegistry) ListTools(_ context.Context, filter toolstore.ListFilter) ([]toolstore.Tool, error) {
	if registry.listToolsError != nil {
		return nil, registry.listToolsError
	}
	if filter.Kind != toolstore.KindHarness {
		registry.t.Fatalf("the fake tool-store serves kind=harness only; asked for %+v", filter)
	}
	rows := registry.harnessTools
	if rows == nil {
		rows = harnessToolRowsForTests
	}
	matching := []map[string]any{}
	for _, row := range rows {
		if filter.Harness == "" || row["harness"] == filter.Harness {
			matching = append(matching, row)
		}
	}
	return registry.toolsFromRows(matching), nil
}

func (registry *fakeToolRegistry) ListInstanceTools(_ context.Context, instanceID string) ([]toolstore.Tool, error) {
	if registry.listInstanceToolsError != nil {
		return nil, registry.listInstanceToolsError
	}
	return registry.toolsFromRows(registry.instanceTools[instanceID]), nil
}

func (registry *fakeToolRegistry) Provision(_ context.Context, request toolstore.ProvisionRequest) (*toolstore.ProvisionResponse, error) {
	registry.t.Helper()
	body := map[string]any{}
	if len(request.Tools) > 0 {
		body["tools"] = request.Tools
	}
	if len(request.ToolIDs) > 0 {
		body["tool_ids"] = request.ToolIDs
	}
	if request.InstanceID != "" {
		body["instance_id"] = request.InstanceID
	}
	encoded, _ := json.Marshal(body)
	var asReceived map[string]any
	_ = json.Unmarshal(encoded, &asReceived)
	registry.provisionBodies = append(registry.provisionBodies, asReceived)
	if registry.provisionError != nil {
		return nil, registry.provisionError
	}
	answer := ""
	if registry.provisionAnswer != nil {
		answer = registry.provisionAnswer(request)
	} else {
		servers := map[string]any{}
		for _, id := range request.ToolIDs {
			servers["tool-"+jsonNumber(id)] = map[string]any{"command": "x"}
		}
		if request.InstanceID != "" {
			servers["instance-default"] = map[string]any{"command": "y"}
		}
		encodedServers, _ := json.Marshal(map[string]any{"mcpServers": servers})
		answer = string(encodedServers)
	}
	var response toolstore.ProvisionResponse
	if err := json.Unmarshal([]byte(answer), &response); err != nil {
		registry.t.Fatalf("fake tool-store provision answer %q: %v", answer, err)
	}
	return &response, nil
}

func (registry *fakeToolRegistry) RecordObservedHarnessTools(_ context.Context, request toolstore.ObservedHarnessToolsRequest) (toolstore.ObservedHarnessToolsResponse, error) {
	registry.observed = append(registry.observed, request)
	if registry.recordObservedError != nil {
		return toolstore.ObservedHarnessToolsResponse{}, registry.recordObservedError
	}
	return toolstore.ObservedHarnessToolsResponse{Created: registry.created, Seen: len(request.ToolNames)}, nil
}

func jsonNumber(number int64) string {
	encoded, _ := json.Marshal(number)
	return string(encoded)
}

// provisionAnswering is a provisionAnswer that answers every request with body.
func provisionAnswering(body string) func(toolstore.ProvisionRequest) string {
	return func(toolstore.ProvisionRequest) string { return body }
}

// fakeBundleResolver stands in for the bundle-store module: it knows one
// bundle id and resolves it to resolution, and answers any other id as
// bundle-store does a missing one. With err set every call fails, as a store
// that could not answer did.
type fakeBundleResolver struct {
	bundleID     string
	resolution   *bundleclient.Resolution
	err          error
	resolveCalls []string
}

func (resolver *fakeBundleResolver) CheckExists(_ context.Context, bundleID string) error {
	if resolver.err != nil {
		return resolver.err
	}
	if _, err := bundleclient.ParseID(bundleID); err != nil {
		return fmt.Errorf("%w: %v", bundleclient.ErrNotFound, err)
	}
	if bundleID != resolver.bundleID {
		return fmt.Errorf("%w: bundle-store has no bundle %s", bundleclient.ErrNotFound, bundleID)
	}
	return nil
}

func (resolver *fakeBundleResolver) Resolve(_ context.Context, bundleID string) (*bundleclient.Resolution, error) {
	resolver.resolveCalls = append(resolver.resolveCalls, bundleID)
	if resolver.err != nil {
		return nil, resolver.err
	}
	if _, err := bundleclient.ParseID(bundleID); err != nil {
		return nil, fmt.Errorf("%w: %v", bundleclient.ErrNotFound, err)
	}
	if bundleID != resolver.bundleID {
		return nil, fmt.Errorf("%w: bundle not applicable: bundle id %s does not exist", bundleclient.ErrNotFound, bundleID)
	}
	return resolver.resolution, nil
}

// memberRefs names each id, for a fake resolution.
func memberRefs(name string, ids ...int64) []bundlestore.MemberRef {
	refs := []bundlestore.MemberRef{}
	for _, id := range ids {
		refs = append(refs, bundlestore.MemberRef{ID: id, Name: name})
	}
	return refs
}
