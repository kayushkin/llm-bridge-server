-- The registry of tools that can be given to a harness (Claude Code, codex,
-- inber and the rest) through llm-bridge-server. Four kinds; Kinds in tool.go
-- is the list, and a test holds the CHECK below to it:
--   mcp     — an external MCP server (stdio, http or sse), started by the harness
--   cli     — any executable, run with a template-substituted argv
--   local   — a Go function compiled into this server
--   harness — a built-in tool of an agent harness (Claude Code's Read); the
--             harness runs it, the store records it and its enabled flag
--
-- id is an identity column: tool ids are plain integers that grant-store and
-- bundle-store hold, so a number is handed out once and never again. The JSON
-- columns are text holding what the store encodes, as the SQLite store kept.

CREATE TABLE tools (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name              TEXT UNIQUE NOT NULL,
    display_name      TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    kind              TEXT NOT NULL,

    input_schema      TEXT NOT NULL DEFAULT '',   -- JSON object, optional for mcp
    env_keys          TEXT NOT NULL DEFAULT '',   -- JSON array of required env var names
    credentials       TEXT NOT NULL DEFAULT '',   -- JSON object: env-var name -> auth-store provider
    tags              TEXT NOT NULL DEFAULT '',   -- JSON array

    -- mcp
    mcp_transport     TEXT NOT NULL DEFAULT '',   -- 'stdio' | 'http' | 'sse'
    mcp_command       TEXT NOT NULL DEFAULT '',
    mcp_args          TEXT NOT NULL DEFAULT '',   -- JSON array
    mcp_url           TEXT NOT NULL DEFAULT '',

    -- cli
    cli_command       TEXT NOT NULL DEFAULT '',
    cli_args_template TEXT NOT NULL DEFAULT '',   -- JSON array; supports {{var}} substitution
    cli_working_dir   TEXT NOT NULL DEFAULT '',
    cli_timeout_ms    INTEGER NOT NULL DEFAULT 0,

    -- local
    local_symbol      TEXT NOT NULL DEFAULT '',

    -- harness
    harness           TEXT NOT NULL DEFAULT '',   -- llm-bridge harness id, e.g. 'claude_code'
    harness_tool_name TEXT NOT NULL DEFAULT '',   -- the harness's own name, e.g. 'Read'
    last_seen_at      BIGINT NOT NULL DEFAULT 0,  -- unix seconds a harness last reported it; 0 = never

    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        BIGINT NOT NULL,
    updated_at        BIGINT NOT NULL,

    CHECK (kind IN ('mcp', 'cli', 'local', 'harness')),
    CHECK ((kind = 'harness') = (harness <> '' AND harness_tool_name <> '')),
    CHECK (kind <> 'harness' OR name = harness || '.' || harness_tool_name),
    CHECK (kind = 'harness' OR (harness = '' AND harness_tool_name = ''))
);

CREATE INDEX idx_tools_kind    ON tools(kind);
CREATE INDEX idx_tools_enabled ON tools(enabled);
CREATE INDEX idx_tools_harness ON tools(harness);

-- instance_tools: per-instance opt-in. A row means this tool is enabled for
-- this harness instance. The global enabled flag on tools is master: a row
-- here means nothing while its tool is off (the store refuses such opt-ins,
-- and every read joins on enabled).
CREATE TABLE instance_tools (
    instance_id  TEXT   NOT NULL,
    tool_id      BIGINT NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
    created_at   BIGINT NOT NULL,
    PRIMARY KEY (instance_id, tool_id)
);

CREATE INDEX idx_instance_tools_tool ON instance_tools(tool_id);
