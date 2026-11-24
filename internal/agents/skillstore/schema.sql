-- skill-store schema
-- Registry of Claude Code / Agent SKILL.md skills, ingested from upstream repos.

-- ⚠️ This line is NOT what enforces the foreign keys below, and it must not be
-- read as if it were. foreign_keys is a per-connection setting and this file is
-- executed once, through a *sql.DB pool, so it reaches whichever single
-- connection served the migration. Measured on this schema, 8 connections held
-- open at once: with this line alone, 1 of 8 enforced. It reads back as 1 on the
-- connection that ran it, so checking the pragma there confirms nothing.
--
-- The mechanism is the "_foreign_keys=on" parameter in the DSN in store.go,
-- which the driver applies to every connection it opens (8 of 8 measured).
-- TestEveryPooledConnectionEnforcesForeignKeys pins that. Keep this line for a
-- direct sqlite3 session on the file; do not treat it as the guarantee.
PRAGMA foreign_keys = ON;

-- ============================================
-- SOURCES (upstream repos that ship skills)
-- ============================================

-- NOTE: this file only ever runs as CREATE TABLE IF NOT EXISTS, so it reaches a
-- fresh database and no other. A column added here alone is invisible to every
-- database that already has the table. Add it to the migrations in store.go too.
CREATE TABLE IF NOT EXISTS sources (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT UNIQUE NOT NULL,            -- e.g. "anthropics", "antigravity"
    kind TEXT NOT NULL DEFAULT 'git',     -- 'git' (clone a remote) | 'local' (read a directory on this host)
    local_path TEXT NOT NULL DEFAULT '',  -- directory to read for kind='local'; must not overlap the install dir
    repo_url TEXT NOT NULL,                -- https://github.com/anthropics/skills.git; empty for kind='local'
    branch TEXT NOT NULL DEFAULT 'main',
    subpath TEXT NOT NULL DEFAULT '',      -- directory within repo where skills live, e.g. "skills"
    trust_tier TEXT NOT NULL DEFAULT 'community',  -- 'official' | 'community' | 'personal'
    license TEXT,
    last_synced_at INTEGER,
    last_commit TEXT,
    last_skill_count INTEGER DEFAULT 0,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- ============================================
-- SKILLS (individual SKILL.md entries)
-- ============================================

CREATE TABLE IF NOT EXISTS skills (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id INTEGER NOT NULL,
    name TEXT NOT NULL,                    -- from SKILL.md frontmatter, e.g. "skill-creator"
    display_name TEXT,
    description TEXT,
    skill_md TEXT NOT NULL,                -- full SKILL.md content
    source_path TEXT NOT NULL,             -- path within source repo, e.g. "skills/skill-creator"
    source_commit TEXT,                    -- git SHA the skill was pulled from
    content_hash TEXT NOT NULL,            -- sha256 of SKILL.md body
    file_count INTEGER NOT NULL DEFAULT 0, -- sibling files (scripts, resources)
    tags TEXT,                              -- JSON array of strings
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(source_id, name),
    FOREIGN KEY (source_id) REFERENCES sources(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_skills_source ON skills(source_id);
CREATE INDEX IF NOT EXISTS idx_skills_name ON skills(name);

-- ============================================
-- INSTALLATIONS (skill → target dir under ~/.claude/skills)
-- ============================================
-- One row per installed skill. target_path is the absolute install directory
-- (e.g. ~/.claude/skills/skill-creator). UNIQUE(target_path) prevents two
-- skills from different sources clobbering the same target.

CREATE TABLE IF NOT EXISTS installations (
    skill_id INTEGER PRIMARY KEY,
    target_path TEXT NOT NULL UNIQUE,
    installed_at INTEGER NOT NULL,
    FOREIGN KEY (skill_id) REFERENCES skills(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_installations_target ON installations(target_path);

-- ============================================
-- SEED FILE VERSIONS (history of bytes for installed skill files)
-- ============================================
-- Captures bytes a runner reports when its on-disk hash diverges from the
-- canonical (bridge-host) content of an installed skill file. Also records
-- "ingest" snapshots whenever the bridge-host install dir gains a new file
-- hash, so the manifest can hand the runner a stable version_id to fetch.
--
-- relpath is the path within the installation, e.g. "SKILL.md" or
-- "scripts/run.sh"; tied to skill_id so renames across skills don't collide.

CREATE TABLE IF NOT EXISTS seed_file_versions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    skill_id INTEGER NOT NULL,
    relpath TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    size INTEGER NOT NULL,
    content_bytes BLOB NOT NULL,
    source TEXT NOT NULL,         -- "ingest", "runner-drift", "scan-import"
    machine_id TEXT,
    note TEXT,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (skill_id) REFERENCES skills(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_seed_file_versions_skill ON seed_file_versions(skill_id, relpath, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_seed_file_versions_sha ON seed_file_versions(skill_id, relpath, sha256);

-- ============================================
-- MACHINE SEED PROFILES (mirror of agent-store's per-runner opt-in)
-- ============================================
-- For skills the only choice today is "all installed skills" yes/no. The row
-- exists per-machine so opt-out is possible without code changes.

CREATE TABLE IF NOT EXISTS machine_seed_profiles (
    machine_id TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- ============================================
-- MACHINE SEED STATE (per-machine, per-file last-known hash)
-- ============================================

CREATE TABLE IF NOT EXISTS machine_seed_state (
    machine_id TEXT NOT NULL,
    skill_id INTEGER NOT NULL,
    relpath TEXT NOT NULL,
    observed_sha TEXT,
    observed_at INTEGER,
    last_pushed_version_id INTEGER,
    last_pushed_at INTEGER,
    PRIMARY KEY (machine_id, skill_id, relpath),
    FOREIGN KEY (skill_id) REFERENCES skills(id) ON DELETE CASCADE,
    FOREIGN KEY (last_pushed_version_id) REFERENCES seed_file_versions(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_machine_seed_state_machine ON machine_seed_state(machine_id);
