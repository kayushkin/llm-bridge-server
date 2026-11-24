package toolstore

import (
	"context"
	"errors"
	"fmt"
)

// ProvisionRequest says which tools to provision, in one of three ways.
//
// Name them outright — by id in ToolIDs, or by name in Tools — or name an
// instance in InstanceID and let the store read that instance's opt-in list
// (the rows the Tools page writes through /instances/{id}/tools/by-name/{name}).
// Exactly one of the three must be set: an empty request is an error — the
// caller must say what it wants, no implicit "all" — and a request carrying
// more than one is an error too, because merging a standing preference with
// a per-call list gives the same field two sources of truth.
//
// ToolIDs is the form for a caller that already holds ids from another store
// — llm-bridge-server, intersecting a principal's grants with an instance's
// opt-ins, has ids on both sides and no reason to go through a name.
type ProvisionRequest struct {
	Tools      []string `json:"tools"`
	ToolIDs    []int64  `json:"tool_ids"`
	InstanceID string   `json:"instance_id"`
}

// ProvisionResponse mirrors the shape Claude Code expects in --mcp-config:
//
//	{ "mcpServers": { "<name>": { "command": ..., "args": ..., "env": {...} } } }
//
// For non-stdio MCP servers, "command" is omitted and "url" + "type" describe
// the http/sse endpoint instead.
type ProvisionResponse struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers,omitempty"`
}

// MCPServerConfig is a single entry under "mcpServers". Field choice depends
// on the source MCPSpec.Transport.
type MCPServerConfig struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Type    string            `json:"type,omitempty"`
}

// Provision builds the MCP server config for the requested tools. Errors
// out (no fallback) if any tool is missing, disabled, or has env-key
// resolution gaps.
func Provision(ctx context.Context, s *Store, req ProvisionRequest, resolve ResolveCredentialFunc) (*ProvisionResponse, error) {
	tools, err := resolveRequestedTools(s, req)
	if err != nil {
		return nil, err
	}
	out := &ProvisionResponse{MCPServers: map[string]MCPServerConfig{}}
	for _, t := range tools {
		name := t.Name
		if t.Kind == KindHarness {
			return nil, fmt.Errorf("provision: tool %q is a built-in tool of the %s harness; the harness provides it, so leave it out of a provision request (only mcp tools are provisionable here)", name, t.Harness)
		}
		if !t.Enabled {
			return nil, fmt.Errorf("provision: tool %q is disabled", name)
		}
		if t.Kind != KindMCP {
			return nil, fmt.Errorf("provision: tool %q has kind %q; only mcp tools are provisionable here", name, t.Kind)
		}
		if t.MCP == nil {
			return nil, fmt.Errorf("provision: tool %q has no mcp spec", name)
		}

		env, err := resolveEnv(ctx, t, resolve)
		if err != nil {
			return nil, err
		}

		cfg := MCPServerConfig{Env: env}
		switch t.MCP.Transport {
		case "stdio":
			cfg.Command = t.MCP.Command
			cfg.Args = t.MCP.Args
		case "http", "sse":
			cfg.URL = t.MCP.URL
			cfg.Type = t.MCP.Transport
		default:
			return nil, fmt.Errorf("provision: tool %q has unknown mcp transport %q", name, t.MCP.Transport)
		}
		out.MCPServers[name] = cfg
	}
	return out, nil
}

// resolveRequestedTools turns a ProvisionRequest into the tools to build
// config for, rejecting a request that names no source or more than one.
//
// The paths deliberately treat a non-MCP tool differently. A caller that
// names tools outright — by id or by name — gets an error from Provision if
// one of them is a CLI or an in-process local, because it asked for something
// this endpoint cannot hand back. An instance's opt-in list is not a
// provisioning request — it is a standing preference that spans every kind
// of tool the instance may use — so the MCP subset is selected out of it and
// the rest left alone. Erroring there would mean one ticked CLI tool wedges
// MCP provisioning for that instance.
func resolveRequestedTools(s *Store, req ProvisionRequest) ([]*Tool, error) {
	sources := 0
	for _, set := range []bool{len(req.Tools) > 0, len(req.ToolIDs) > 0, req.InstanceID != ""} {
		if set {
			sources++
		}
	}
	switch {
	case sources > 1:
		return nil, errors.New("provision: request names more than one of tools, tool_ids and instance_id; pick one")
	case len(req.Tools) > 0:
		tools := make([]*Tool, 0, len(req.Tools))
		for _, name := range req.Tools {
			t, err := s.GetToolByName(name)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil, fmt.Errorf("provision: tool %q not found", name)
				}
				return nil, fmt.Errorf("provision: lookup %q: %w", name, err)
			}
			tools = append(tools, t)
		}
		return tools, nil
	case len(req.ToolIDs) > 0:
		tools := make([]*Tool, 0, len(req.ToolIDs))
		for _, id := range req.ToolIDs {
			t, err := s.GetTool(id)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil, fmt.Errorf("provision: tool id %d not found", id)
				}
				return nil, fmt.Errorf("provision: lookup id %d: %w", id, err)
			}
			tools = append(tools, t)
		}
		return tools, nil
	case req.InstanceID != "":
		listed, err := s.ListInstanceTools(req.InstanceID)
		if err != nil {
			return nil, fmt.Errorf("provision: list tools for instance %q: %w", req.InstanceID, err)
		}
		var tools []*Tool
		for i := range listed {
			if listed[i].Kind == KindMCP {
				tools = append(tools, &listed[i])
			}
		}
		return tools, nil
	default:
		return nil, errors.New("provision: one of tools, tool_ids or instance_id is required")
	}
}

// resolveEnv walks a tool's EnvKeys and returns the map to embed in the MCP
// server config. Each env key MUST be mapped in t.Credentials to an
// auth-store provider; the resolver fetches the value. Missing mappings or
// resolver failures are errors — no silent fallback to os.Getenv.
func resolveEnv(ctx context.Context, t *Tool, resolve ResolveCredentialFunc) (map[string]string, error) {
	if len(t.EnvKeys) == 0 {
		return nil, nil
	}
	if resolve == nil {
		return nil, fmt.Errorf("provision: tool %q needs env keys %v but no credential resolver is configured", t.Name, t.EnvKeys)
	}
	out := make(map[string]string, len(t.EnvKeys))
	for _, k := range t.EnvKeys {
		provider, ok := t.Credentials[k]
		if !ok || provider == "" {
			return nil, fmt.Errorf("provision: tool %q env key %q has no credentials mapping", t.Name, k)
		}
		v, err := resolve(ctx, provider)
		if err != nil {
			return nil, fmt.Errorf("provision: tool %q resolve %q (provider %q): %w", t.Name, k, provider, err)
		}
		out[k] = v
	}
	return out, nil
}
