package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/agents/tools/localtools"
)

// SeedRegistry writes the rows this server's code owns, as the standalone
// tool-store did at every start and in the same order: every in-process tool
// compiled in (kind local), the curated MCP servers, and the built-in tools of
// Claude Code and Codex. Each step keeps an operator's enabled flag, and the
// harness step only adds rows that are missing. The tools that reach outside
// services are in localtools.All() only once the server has registered them with
// localtools.RegisterToolsThatReachOutsideServices, so it does that first.
func SeedRegistry(ctx context.Context, s *Store) error {
	if err := seedLocalTools(ctx, s); err != nil {
		return fmt.Errorf("seed local tools: %w", err)
	}
	if err := seedMCPTools(ctx, s); err != nil {
		return fmt.Errorf("seed mcp tools: %w", err)
	}
	if err := seedHarnessTools(ctx, s); err != nil {
		return fmt.Errorf("seed harness tools: %w", err)
	}
	return nil
}

// seedLocalTools upserts every in-process registered tool as a kind=local row.
// New rows default to enabled=false — operators (or eventually bridge-ui) opt
// each tool in explicitly. Existing rows preserve their enabled state, so a
// user-enabled tool stays enabled across restarts and a re-disabled one stays
// disabled. Description and schema are always refreshed from code.
func seedLocalTools(ctx context.Context, store *Store) error {
	for _, impl := range localtools.All() {
		schemaJSON, err := json.Marshal(impl.InputSchema)
		if err != nil {
			return fmt.Errorf("marshal schema for %s: %w", impl.Name, err)
		}
		enabled := false
		if existing, err := store.GetToolByName(ctx, impl.Name); err == nil {
			enabled = existing.Enabled
		} else if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("lookup local tool %s: %w", impl.Name, err)
		}
		t := &Tool{
			Name:        impl.Name,
			Description: impl.Description,
			Kind:        KindLocal,
			InputSchema: schemaJSON,
			Local:       &LocalSpec{Symbol: impl.Name},
			Enabled:     enabled,
		}
		if _, err := store.UpsertTool(ctx, t); err != nil {
			return fmt.Errorf("upsert local tool %s: %w", impl.Name, err)
		}
	}
	return nil
}

// InvokeRegisteredLocalTool runs the in-process tool registered as name with
// input, the JSON body of POST /tools/by-name/{name}/invoke. It is the
// InvokeLocal a server hands NewHandlers.
func InvokeRegisteredLocalTool(ctx context.Context, name, input string) (string, error) {
	impl, ok := localtools.ByName(name)
	if !ok {
		return "", fmt.Errorf("local tool %q not registered", name)
	}
	return impl.Run(ctx, input)
}

// RegisteredLocalTools describes every in-process tool compiled in, for
// GET /locals. It is the ListLocals a server hands NewHandlers.
func RegisteredLocalTools() []LocalDescriptor {
	out := make([]LocalDescriptor, 0, len(localtools.All()))
	for _, impl := range localtools.All() {
		schemaJSON, _ := json.Marshal(impl.InputSchema)
		out = append(out, LocalDescriptor{
			Name:        impl.Name,
			Description: impl.Description,
			InputSchema: schemaJSON,
		})
	}
	return out
}
