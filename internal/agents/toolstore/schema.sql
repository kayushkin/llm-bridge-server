-- tool-store schema
-- Registry of tools that can be seeded into harnesses (Claude Code, openclaw,
-- jig, codex, inber, etc.) via llm-bridge-server. Four kinds (Kinds in tool.go
-- is the list; a test holds the CHECK below to it):
--   mcp     — external MCP server (stdio/http/sse), spawned by the harness
--   cli     — arbitrary executable invoked via template-substituted argv
--   local   — Go function registered into the tool-store binary at build time
--             (typically agentkit tools)
--   harness — a built-in tool of an agent harness (Claude Code's Read); the
--             harness runs it, tool-store records it and its enabled flag
--
-- A database made before the harness kind has a CHECK without it and no
-- harness columns; Open rebuilds that table (migrate.go). A database made
-- before last_seen_at gets the column added with its default (migrate.go).

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS tools (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT UNIQUE NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL,                 -- 'mcp' | 'cli' | 'local' | 'harness'

    input_schema    TEXT NOT NULL DEFAULT '',      -- JSON object, optional for mcp
    env_keys        TEXT NOT NULL DEFAULT '',      -- JSON array of required env var names
    credentials     TEXT NOT NULL DEFAULT '',      -- JSON object: env-var name -> auth-store provider
    tags            TEXT NOT NULL DEFAULT '',      -- JSON array

    -- mcp
    mcp_transport   TEXT NOT NULL DEFAULT '',      -- 'stdio' | 'http' | 'sse'
    mcp_command     TEXT NOT NULL DEFAULT '',
    mcp_args        TEXT NOT NULL DEFAULT '',      -- JSON array
    mcp_url         TEXT NOT NULL DEFAULT '',

    -- cli
    cli_command       TEXT NOT NULL DEFAULT '',
    cli_args_template TEXT NOT NULL DEFAULT '',    -- JSON array; supports {{var}} substitution
    cli_working_dir   TEXT NOT NULL DEFAULT '',
    cli_timeout_ms    INTEGER NOT NULL DEFAULT 0,

    -- local
    local_symbol    TEXT NOT NULL DEFAULT '',      -- e.g. "agentkit/tools.Shell"

    -- harness
    harness           TEXT NOT NULL DEFAULT '',    -- llm-bridge harness id, e.g. 'claude_code'
    harness_tool_name TEXT NOT NULL DEFAULT '',    -- the harness's own name, e.g. 'Read'
    last_seen_at      INTEGER NOT NULL DEFAULT 0,  -- unix seconds a harness last reported it (POST /harness-tools/observed); 0 = never

    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,

    CHECK (kind IN ('mcp', 'cli', 'local', 'harness')),
    CHECK ((kind = 'harness') = (harness <> '' AND harness_tool_name <> '')),
    CHECK (kind <> 'harness' OR name = harness || '.' || harness_tool_name),
    CHECK (kind = 'harness' OR (harness = '' AND harness_tool_name = ''))
);

CREATE INDEX IF NOT EXISTS idx_tools_kind    ON tools(kind);
CREATE INDEX IF NOT EXISTS idx_tools_enabled ON tools(enabled);
CREATE INDEX IF NOT EXISTS idx_tools_harness ON tools(harness);

-- instance_tools: per-instance opt-in. A row means this tool is enabled for
-- this harness instance. Global enabled flag on `tools` is master — a row
-- here is meaningless if the corresponding tool has enabled=0 (the API
-- refuses such opt-ins; provision queries skip them).
CREATE TABLE IF NOT EXISTS instance_tools (
    instance_id  TEXT    NOT NULL,
    tool_id      INTEGER NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (instance_id, tool_id)
);

CREATE INDEX IF NOT EXISTS idx_instance_tools_instance ON instance_tools(instance_id);
CREATE INDEX IF NOT EXISTS idx_instance_tools_tool     ON instance_tools(tool_id);
