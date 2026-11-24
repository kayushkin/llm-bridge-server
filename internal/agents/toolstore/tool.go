package toolstore

import "encoding/json"

type Kind string

const (
	KindMCP     Kind = "mcp"
	KindCLI     Kind = "cli"
	KindLocal   Kind = "local"
	KindHarness Kind = "harness"
)

// KindDescriptor is one entry of the kind vocabulary served on GET /kinds.
type KindDescriptor struct {
	Kind        Kind   `json:"kind"`
	Description string `json:"description"`
}

// Kinds is the one list of tool kinds. Kind.Valid, validation errors and
// GET /kinds all read it; the CHECK on tools.kind in schema.sql is held to it
// by a test.
var Kinds = []KindDescriptor{
	{KindMCP, "An external MCP server (stdio, http or sse); the harness launches it from the config POST /provision returns."},
	{KindCLI, "A command and argv template; tool-store runs it on POST /tools/by-name/{name}/invoke."},
	{KindLocal, "A Go function compiled into the tool-store binary; tool-store runs it on POST /tools/by-name/{name}/invoke."},
	{KindHarness, "A built-in tool of an agent harness (Claude Code's Read, Codex's shell_tool); the harness runs it, tool-store only records it and whether it is enabled."},
}

func (k Kind) Valid() bool {
	for _, known := range Kinds {
		if known.Kind == k {
			return true
		}
	}
	return false
}

// KindNames returns the kinds in Kinds order, for error messages.
func KindNames() []string {
	names := make([]string, 0, len(Kinds))
	for _, known := range Kinds {
		names = append(names, string(known.Kind))
	}
	return names
}

// HarnessToolRowName is the registry name of a harness tool:
// "<harness>.<harness_tool_name>", e.g. "claude_code.Read". The harness prefix
// keeps it apart from local tools that share a bare name (web_search).
func HarnessToolRowName(harness, harnessToolName string) string {
	return harness + "." + harnessToolName
}

// Tool is a registry entry. Exactly one of MCP/CLI/Local is populated based on
// Kind; a kind=harness tool has none of them and carries Harness (the
// llm-bridge harness id, e.g. "claude_code" or "codex") and HarnessToolName
// (the name the harness itself uses, e.g. "Read"), which every other kind
// leaves empty. EnvKeys lists required environment variable *names* — values are
// resolved at provision time from the canonical credential source, never
// stored here. Credentials (optional) maps each env-var name to the
// auth-store provider that owns its value; entries with no mapping must be
// resolved by the caller out of band (and provisioning fails if neither path
// produces a value — no silent fallback).
type Tool struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name,omitempty"`
	Description string            `json:"description"`
	Kind        Kind              `json:"kind"`
	InputSchema json.RawMessage   `json:"input_schema,omitempty"`
	EnvKeys     []string          `json:"env_keys,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
	Tags        []string          `json:"tags,omitempty"`

	Harness         string `json:"harness,omitempty"`
	HarnessToolName string `json:"harness_tool_name,omitempty"`
	// LastSeenAt is when a harness last reported this tool in its session's
	// tool list (POST /harness-tools/observed), in unix seconds; 0 means never
	// reported. Only that route writes it: POST /tools and seeding leave it.
	LastSeenAt int64 `json:"last_seen_at"`

	MCP   *MCPSpec   `json:"mcp,omitempty"`
	CLI   *CLISpec   `json:"cli,omitempty"`
	Local *LocalSpec `json:"local,omitempty"`

	Enabled   bool  `json:"enabled"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// MCPSpec describes how to launch or reach an MCP server.
type MCPSpec struct {
	Transport string   `json:"transport"`         // stdio | http | sse
	Command   string   `json:"command,omitempty"` // stdio
	Args      []string `json:"args,omitempty"`    // stdio
	URL       string   `json:"url,omitempty"`     // http | sse
}

// CLISpec describes an arbitrary command-line tool. ArgsTemplate elements may
// contain {{var}} placeholders that are substituted from the JSON-decoded input
// at invocation time.
type CLISpec struct {
	Command      string   `json:"command"`
	ArgsTemplate []string `json:"args_template,omitempty"`
	WorkingDir   string   `json:"working_dir,omitempty"`
	TimeoutMs    int      `json:"timeout_ms,omitempty"`
}

// LocalSpec describes a Go function registered into the tool-store binary at
// build time. Symbol is the canonical name (e.g. "agentkit/tools.Shell") used
// to look up the implementation in the in-process registry.
type LocalSpec struct {
	Symbol string `json:"symbol"`
}

// LocalDescriptor describes one in-process registered local tool — what's
// available to be enabled via POST /tools.
type LocalDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}
