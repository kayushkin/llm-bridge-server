-- skill-store: the registry of Claude Code / Agent SKILL.md skills, the
-- installations of them on this host, and the per-machine seeding of installed
-- skill files to runners.
--
-- The text of each SKILL.md and the bytes of every seed file version are kept
-- here. A skill's sibling files (scripts, resources) are not: they stay on disk
-- under <data directory>/skills/<id>/, cloned sources under
-- <data directory>/repos/<name>/, and installs are copied into the install
-- directory.
--
-- Ids are identity numbers and the public ids are those plain numbers, as the
-- standalone store's AUTOINCREMENT ids were: bundle-store members and
-- grant-store hold skill ids, and runners hold version ids from the manifest.
-- Columns that the standalone schema let be NULL stay nullable, so an import
-- copies them as they were.

CREATE TABLE sources (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    kind             TEXT NOT NULL DEFAULT 'git',      -- 'git' (clone a remote) | 'local' (read a directory on this host)
    local_path       TEXT NOT NULL DEFAULT '',         -- directory to read for kind='local'; must not overlap the install dir
    repo_url         TEXT NOT NULL,                    -- empty for kind='local'
    branch           TEXT NOT NULL DEFAULT 'main',
    subpath          TEXT NOT NULL DEFAULT '',
    trust_tier       TEXT NOT NULL DEFAULT 'community', -- 'official' | 'community' | 'personal'
    license          TEXT,
    last_synced_at   BIGINT,
    last_commit      TEXT,
    last_skill_count INTEGER NOT NULL DEFAULT 0,
    enabled          BOOLEAN NOT NULL DEFAULT true,
    created_at       BIGINT NOT NULL,
    updated_at       BIGINT NOT NULL
);

CREATE TABLE skills (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id     BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,                       -- from SKILL.md frontmatter
    display_name  TEXT,
    description   TEXT,
    skill_md      TEXT NOT NULL,                       -- full SKILL.md content
    source_path   TEXT NOT NULL,                       -- path within the source
    source_commit TEXT,
    content_hash  TEXT NOT NULL,                       -- sha256 of SKILL.md
    file_count    INTEGER NOT NULL DEFAULT 0,          -- sibling files, kept on disk
    tags          TEXT,                                -- JSON array of strings
    enabled       BOOLEAN NOT NULL DEFAULT true,
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL,
    UNIQUE (source_id, name)
);

CREATE INDEX skills_source ON skills (source_id);
CREATE INDEX skills_name ON skills (name);

-- One row per installed skill. target_path is the absolute install directory;
-- UNIQUE keeps two skills from different sources from owning one target.
CREATE TABLE installations (
    skill_id     BIGINT PRIMARY KEY REFERENCES skills(id) ON DELETE CASCADE,
    target_path  TEXT NOT NULL UNIQUE,
    installed_at BIGINT NOT NULL
);

-- History of the bytes of installed skill files: "ingest" snapshots of this
-- host's install dir, and "runner-drift" bytes a runner reported.
CREATE TABLE seed_file_versions (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    skill_id      BIGINT NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    relpath       TEXT NOT NULL,
    sha256        TEXT NOT NULL,
    size          BIGINT NOT NULL,
    content_bytes BYTEA NOT NULL,
    source        TEXT NOT NULL,                       -- "ingest", "runner-drift", "scan-import"
    machine_id    TEXT,
    note          TEXT,
    created_at    BIGINT NOT NULL
);

CREATE INDEX seed_file_versions_skill ON seed_file_versions (skill_id, relpath, created_at DESC);
CREATE INDEX seed_file_versions_sha ON seed_file_versions (skill_id, relpath, sha256);

-- Per-machine opt-in to seeding: "all installed skills", yes or no.
CREATE TABLE machine_seed_profiles (
    machine_id TEXT PRIMARY KEY,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);

-- Per machine, per file: the last hash the runner reported.
CREATE TABLE machine_seed_state (
    machine_id             TEXT NOT NULL,
    skill_id               BIGINT NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    relpath                TEXT NOT NULL,
    observed_sha           TEXT,
    observed_at            BIGINT,
    last_pushed_version_id BIGINT REFERENCES seed_file_versions(id) ON DELETE SET NULL,
    last_pushed_at         BIGINT,
    PRIMARY KEY (machine_id, skill_id, relpath)
);

CREATE INDEX machine_seed_state_machine ON machine_seed_state (machine_id);
