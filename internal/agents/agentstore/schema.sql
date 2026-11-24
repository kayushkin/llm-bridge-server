-- agent-store schema
-- Single source of truth for agent identity, nature, and harness configs

PRAGMA foreign_keys = ON;

-- ============================================
-- AGENTS (canonical definitions)
-- ============================================

CREATE TABLE IF NOT EXISTS agents (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    slug TEXT UNIQUE NOT NULL,            -- celtic name: "claxon", "brigid", etc.
    display_name TEXT NOT NULL,           -- "Claxon", "Brigid"
    emoji TEXT,                           -- "🦀", "🔥"
    projects TEXT,                        -- comma-separated: "si,dash"
    description TEXT,
    role TEXT,
    enabled INTEGER DEFAULT 1,
    parent_agent_id INTEGER,              -- nullable; non-null means this is a subagent under another agent
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (parent_agent_id) REFERENCES agents(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_agents_parent ON agents(parent_agent_id);

-- ============================================
-- HARNESS (registry of agent runtimes — CC, Codex, inber, etc.)
-- ============================================
-- Pre-2026-05-11 name: "orchestrators". Renamed to align with the harness-layer
-- design in ~/repos/llm-bridge-server/docs/HARNESS-LAYER.md.

CREATE TABLE IF NOT EXISTS harness (
    id TEXT PRIMARY KEY,                  -- "claudecode", "codex", "inber", "openclaw", "dash"
    display_name TEXT NOT NULL,
    default_agent_id INTEGER,             -- FK to agents.id
    config_path TEXT,                     -- e.g. "~/.openclaw/openclaw.json"
    api_endpoint TEXT,                    -- e.g. "localhost:8101/api/agents"
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (default_agent_id) REFERENCES agents(id) ON DELETE SET NULL
);

-- ============================================
-- AGENT ↔ HARNESS (per-harness agent config)
-- ============================================
-- Pre-2026-05-11 name: "agent_orchestrators".

CREATE TABLE IF NOT EXISTS agent_harness (
    agent_id INTEGER NOT NULL,
    harness_id TEXT NOT NULL,
    harness_agent_id TEXT NOT NULL,        -- what the harness calls this agent
    enabled INTEGER DEFAULT 1,
    model_primary TEXT,
    model_fallbacks TEXT,                  -- JSON array
    workspace_path TEXT,
    thinking_budget INTEGER DEFAULT 0,     -- token budget for extended thinking
    context_budget INTEGER DEFAULT 0,      -- token budget for context assembly
    context_tags TEXT,                     -- JSON array of tags, e.g. ["identity","code"]
    max_turns INTEGER DEFAULT 0,
    max_input_tokens INTEGER DEFAULT 0,
    max_response_time INTEGER DEFAULT 0,   -- seconds
    system_prompt TEXT,                    -- custom system prompt override
    project TEXT,                          -- forge project name
    shelved INTEGER DEFAULT 0,
    is_default INTEGER DEFAULT 0,
    subagent_allow TEXT,                   -- JSON array, e.g. '["*"]'
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (agent_id, harness_id),
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE,
    FOREIGN KEY (harness_id) REFERENCES harness(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_agent_harness_harness ON agent_harness(harness_id);

-- ============================================
-- AGENT HARNESS TOOLS (tool enrollment per agent+harness)
-- ============================================
-- Pre-2026-05-11 name: "agent_tools".

CREATE TABLE IF NOT EXISTS agent_harness_tools (
    agent_id INTEGER NOT NULL,
    harness_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    PRIMARY KEY (agent_id, harness_id, tool_name),
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE,
    FOREIGN KEY (harness_id) REFERENCES harness(id) ON DELETE CASCADE
);

-- ============================================
-- AGENT SKILLS (skill enrollment per agent)
-- ============================================
-- Parallel to agent_harness_tools. Skill enrollment is harness-agnostic at the
-- canonical level; per-harness rendering is in the render library (see
-- ~/repos/llm-bridge-server/docs/TOOL-ROUTING.md skills section).

CREATE TABLE IF NOT EXISTS agent_skills (
    agent_id INTEGER NOT NULL,
    skill_id TEXT NOT NULL,                -- references skill-store entry by id
    PRIMARY KEY (agent_id, skill_id),
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

-- ============================================
-- AGENT SYSTEM PROMPT REFS
-- ============================================

CREATE TABLE IF NOT EXISTS agent_system_prompt_refs (
    harness_id TEXT NOT NULL,
    host_agent_id INTEGER NOT NULL,       -- agent whose prompt contains the listing
    referenced_agent_id INTEGER NOT NULL, -- agent being listed
    prompt_location TEXT NOT NULL,         -- "AGENTS.md", "openclaw.json"
    created_at INTEGER NOT NULL,
    PRIMARY KEY (harness_id, host_agent_id, referenced_agent_id, prompt_location),
    FOREIGN KEY (harness_id) REFERENCES harness(id) ON DELETE CASCADE,
    FOREIGN KEY (host_agent_id) REFERENCES agents(id) ON DELETE CASCADE,
    FOREIGN KEY (referenced_agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

-- ============================================
-- AGENT NATURE (identity/personality)
-- ============================================

CREATE TABLE IF NOT EXISTS agent_nature (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id INTEGER NOT NULL,
    kind TEXT NOT NULL,                    -- identity, principle, value, user, project
    content TEXT NOT NULL,
    content_hash TEXT,                     -- SHA256 of content for quick comparison
    priority INTEGER DEFAULT 0,
    source_path TEXT,                      -- e.g. ~/repos/inber/agents/claxon/soul.md
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_agent_nature_agent ON agent_nature(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_nature_kind ON agent_nature(kind);

-- ============================================
-- AGENT NAME ALIASES
-- ============================================

CREATE TABLE IF NOT EXISTS agent_name_aliases (
    agent_id INTEGER NOT NULL,
    alias TEXT NOT NULL,
    context TEXT,                           -- "openclaw", "dash", "logstack"
    created_at INTEGER NOT NULL,
    PRIMARY KEY (agent_id, alias),
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_aliases_alias ON agent_name_aliases(alias);

-- ============================================
-- FILE DISTRIBUTIONS (agent-store → harness files)
-- ============================================

CREATE TABLE IF NOT EXISTS file_distributions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id INTEGER NOT NULL,
    harness_id TEXT NOT NULL,
    file_path TEXT NOT NULL,               -- absolute path where file was written
    content_hash TEXT NOT NULL,            -- SHA256 of file content at distribution time
    distributed_at INTEGER NOT NULL,
    source_nature_ids TEXT,                -- JSON array of agent_nature IDs that contributed
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE,
    FOREIGN KEY (harness_id) REFERENCES harness(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_file_dist_agent ON file_distributions(agent_id);
CREATE INDEX IF NOT EXISTS idx_file_dist_harness ON file_distributions(harness_id);
CREATE INDEX IF NOT EXISTS idx_file_dist_path ON file_distributions(file_path);

-- ============================================
-- FILE SCANS (drift detection)
-- ============================================

CREATE TABLE IF NOT EXISTS file_scans (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_distribution_id INTEGER NOT NULL,
    scanned_at INTEGER NOT NULL,
    current_hash TEXT NOT NULL,             -- hash at scan time
    status TEXT NOT NULL,                   -- "unchanged", "modified", "missing", "new"
    diff_summary TEXT,                      -- brief description of changes
    ingested INTEGER DEFAULT 0,            -- whether changes were pulled back into agent_nature
    FOREIGN KEY (file_distribution_id) REFERENCES file_distributions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_file_scans_dist ON file_scans(file_distribution_id);
CREATE INDEX IF NOT EXISTS idx_file_scans_status ON file_scans(status);

-- ============================================
-- PROJECTS
-- ============================================

CREATE TABLE IF NOT EXISTS projects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    slug TEXT UNIQUE NOT NULL,
    name TEXT NOT NULL,
    path TEXT,
    description TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS project_nature (
    project_id INTEGER NOT NULL,
    nature_id INTEGER NOT NULL,
    priority INTEGER DEFAULT 0,
    PRIMARY KEY (project_id, nature_id),
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE,
    FOREIGN KEY (nature_id) REFERENCES agent_nature(id) ON DELETE CASCADE
);

-- ============================================
-- AGENT STATUS (runtime status tracking)
-- ============================================

CREATE TABLE IF NOT EXISTS agent_status (
    agent_id INTEGER NOT NULL,
    harness_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'idle',
    task TEXT,
    session_id TEXT,
    started_at INTEGER,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (agent_id, harness_id),
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE,
    FOREIGN KEY (harness_id) REFERENCES harness(id) ON DELETE CASCADE
);

-- ============================================
-- TRACKED FILES (disk-is-truth index for CC/agent config files)
-- ============================================
-- path = canonical path (without .disabled suffix).
-- enabled=0 means the file lives at path+".disabled" on disk.
-- DB stores no content; GET /files/{id}/content reads from disk live.
-- scope values: global, project, subagent, memory, command, inber, mcp-config, settings

CREATE TABLE IF NOT EXISTS tracked_files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT UNIQUE NOT NULL,
    scope TEXT NOT NULL,                    -- global, project, subagent, memory, inber, mcp-config, settings
    agent_slug TEXT,                        -- nullable; matched from filename stem
    enabled INTEGER NOT NULL DEFAULT 1,
    fs_hash TEXT,
    size INTEGER,
    mtime INTEGER,
    last_scanned_at INTEGER,
    status TEXT NOT NULL DEFAULT 'present', -- present, missing
    ignored_by_rule_id INTEGER,             -- tracked_file_ignore_rules.id; NULL = not ignored
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tracked_files_scope ON tracked_files(scope);
CREATE INDEX IF NOT EXISTS idx_tracked_files_agent ON tracked_files(agent_slug);
CREATE INDEX IF NOT EXISTS idx_tracked_files_status ON tracked_files(status);

-- ============================================
-- TRACKED FILE VERSIONS (full history)
-- ============================================
-- Every UI save, scan-detected drift, and runner-reported drift creates a row.
-- content_bytes is the raw file body at that version. Versions are kept forever;
-- pruning is a separate decision.
--
-- source values:
--   "ui-save"        — user saved via PUT /files/{id}/content
--   "scan-import"    — bridge auto-scan picked up a hash change made out-of-band
--   "runner-drift"   — a runner reported a hash on its disk that the bridge did not know
--   "seed"           — a runner accepted a seeded version (recorded for audit)
--   "harness-render" — a harness bridge EnsureAgent produced this content
--
-- machine_id is the harness-store machine ID for runner-drift / seed rows;
-- NULL for ui-save, bridge-host scan-import, and harness-render.

CREATE TABLE IF NOT EXISTS tracked_file_versions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    tracked_file_id INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    size INTEGER NOT NULL,
    content_bytes BLOB NOT NULL,
    source TEXT NOT NULL,
    machine_id TEXT,
    note TEXT,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (tracked_file_id) REFERENCES tracked_files(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_tracked_file_versions_file ON tracked_file_versions(tracked_file_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tracked_file_versions_sha ON tracked_file_versions(tracked_file_id, sha256);

-- ============================================
-- STRUCTURED PROMPT SOURCE
-- ============================================
-- The prompt is stored as sections, and a section names no harness and no
-- file: every harness receives the same sections. Which files a collection
-- renders to is a separate fact (prompt_collection_outputs), and whether the
-- bridge injects the prompt or leaves a harness to read its own file is
-- another (prompt_harness_deliveries). The rendered files stay in
-- tracked_files / tracked_file_versions.

CREATE TABLE IF NOT EXISTS prompt_collections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    slug TEXT UNIQUE NOT NULL,
    title TEXT NOT NULL,
    scope TEXT NOT NULL,                   -- global, project
    root_path TEXT NOT NULL,               -- $HOME for global, repo root for project
    description TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    retired_at INTEGER NOT NULL DEFAULT 0  -- RetirePromptCollection; 0 while in use
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_prompt_collections_scope_root ON prompt_collections(scope, root_path);

CREATE TABLE IF NOT EXISTS prompt_sections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    collection_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    heading TEXT NOT NULL DEFAULT '',      -- the markdown heading line, '' for a preamble
    body TEXT NOT NULL,
    tags TEXT NOT NULL DEFAULT '[]',       -- JSON array of strings
    position INTEGER NOT NULL DEFAULT 0,   -- render order within the collection
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (collection_id) REFERENCES prompt_collections(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_prompt_sections_collection_position ON prompt_sections(collection_id, position, id);

-- Append-only. One row per write to a section, holding the state the write
-- left behind (for a delete: the state it removed). No foreign key to
-- prompt_sections, so the history of a deleted section survives it.
CREATE TABLE IF NOT EXISTS prompt_section_revisions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    section_id INTEGER NOT NULL,
    collection_id INTEGER NOT NULL,
    operation TEXT NOT NULL,               -- create, update, delete
    title TEXT NOT NULL,
    heading TEXT NOT NULL,
    body TEXT NOT NULL,
    tags TEXT NOT NULL,
    position INTEGER NOT NULL,
    enabled INTEGER NOT NULL,
    source TEXT NOT NULL,                  -- import, ui, drift
    drift_id INTEGER,                      -- prompt_drifts.id when source = drift
    note TEXT,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_prompt_section_revisions_section ON prompt_section_revisions(section_id, id);
CREATE INDEX IF NOT EXISTS idx_prompt_section_revisions_collection ON prompt_section_revisions(collection_id, id);

-- A file one collection renders to. accounted_sha256 is the hash of the
-- last on-disk content the sections are known to account for: set when the
-- file is imported, rendered, or a drift from it is applied. A disk hash that
-- differs from it is drift, and a render refuses to write over drift.
CREATE TABLE IF NOT EXISTS prompt_collection_outputs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    collection_id INTEGER NOT NULL,
    relative_path TEXT NOT NULL,           -- relative to the collection's root_path
    enabled INTEGER NOT NULL DEFAULT 1,
    accounted_sha256 TEXT,
    accounted_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (collection_id, relative_path),
    FOREIGN KEY (collection_id) REFERENCES prompt_collections(id) ON DELETE CASCADE
);

-- How the bridge hands the prompt to one harness. 'inject' = the bridge puts
-- the resolved prompt in the system prompt at spawn. 'native_file' = the
-- harness reads a rendered file itself and the bridge must not inject, or the
-- harness would get the prompt twice. harness is the id the bridge uses.
CREATE TABLE IF NOT EXISTS prompt_harness_deliveries (
    harness TEXT PRIMARY KEY,
    delivery TEXT NOT NULL,                -- inject, native_file
    native_relative_path TEXT,             -- native_file only: the file the harness reads, relative to a collection root
    note TEXT,
    updated_at INTEGER NOT NULL
);

-- An output file whose on-disk content no longer matches what the sections
-- account for. disk_content is kept so the edit survives whatever happens to
-- the file next.
CREATE TABLE IF NOT EXISTS prompt_drifts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    collection_id INTEGER NOT NULL,
    output_id INTEGER NOT NULL,
    accounted_sha256 TEXT,                 -- what the sections accounted for when the drift was seen
    disk_sha256 TEXT NOT NULL,
    disk_content BLOB NOT NULL,
    status TEXT NOT NULL,                  -- open, held, applied, dismissed, superseded, already_accounted
    held_reason TEXT,
    operations TEXT,                       -- JSON: the section operations that reproduce the disk content
    annotation TEXT,                       -- JSON: what the tagging agent added (tags, titles, note)
    created_at INTEGER NOT NULL,
    resolved_at INTEGER,
    UNIQUE (output_id, disk_sha256),
    FOREIGN KEY (collection_id) REFERENCES prompt_collections(id) ON DELETE CASCADE,
    FOREIGN KEY (output_id) REFERENCES prompt_collection_outputs(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_prompt_drifts_status ON prompt_drifts(status, id);

-- Paths the scan finds but that are copies or third-party files, not prompts
-- anyone here maintains. A matching row is stamped, never deleted.
CREATE TABLE IF NOT EXISTS tracked_file_ignore_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,                    -- path_pattern, git_worktree
    path_pattern TEXT NOT NULL DEFAULT '', -- path_pattern rules only: relative to $HOME, '*' matches any run of characters, '/' included
    reason TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    UNIQUE (kind, path_pattern)
);

-- ============================================
-- MACHINE SEED PROFILES (per-runner opt-in scopes)
-- ============================================
-- One row per machine. scopes is a JSON array of scope names the runner
-- pulls down. Default on enroll = ["global","subagent","memory","command","inber"]
-- (all scopes except "project", whose paths are bridge-host-local).

CREATE TABLE IF NOT EXISTS machine_seed_profiles (
    machine_id TEXT PRIMARY KEY,
    scopes TEXT NOT NULL DEFAULT '["global","subagent","memory","command","inber"]',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- ============================================
-- MACHINE SEED STATE (per-machine, per-file last-known hash)
-- ============================================
-- Tracks what hash a given runner most recently has on disk for a given
-- tracked_file. The runner reports its observed hash on every reconcile
-- (full or push); the bridge updates this row. When the bridge needs to
-- push a new version, it consults this row to know what the runner thinks
-- the file currently is — that's the "last-seeded hash" the runner uses
-- for its pre-overwrite drift check. NULL observed_sha = never seeded.

CREATE TABLE IF NOT EXISTS machine_seed_state (
    machine_id TEXT NOT NULL,
    tracked_file_id INTEGER NOT NULL,
    observed_sha TEXT,
    observed_at INTEGER,
    last_pushed_version_id INTEGER,
    last_pushed_at INTEGER,
    PRIMARY KEY (machine_id, tracked_file_id),
    FOREIGN KEY (tracked_file_id) REFERENCES tracked_files(id) ON DELETE CASCADE,
    FOREIGN KEY (last_pushed_version_id) REFERENCES tracked_file_versions(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_machine_seed_state_machine ON machine_seed_state(machine_id);
