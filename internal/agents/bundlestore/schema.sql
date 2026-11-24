-- bundle-store schema
-- Curated "bundles": named, inheritable sets of {skills, tools} (plus optional
-- model/effort defaults) selected for a session by repo + task tags. A bundle
-- is a CROSS-STORE join artifact — its members are the *ids* skill-store and
-- tool-store assigned, carried with the name alongside for display only.
-- bundle-store owns the curated definitions and the /resolve composition; it
-- does not own the underlying skills or tools.

PRAGMA foreign_keys = ON;

-- `extends_id` is the parent bundle's id, not its name. bundle-store owns the
-- bundles table, so it is also the store that assigns that id — a child asks it
-- once at write time and records what it got back. Storing the parent's name
-- instead would put a name join inside this table: `bundles.name` is UNIQUE so
-- the lookup could never pick the wrong row, but renaming a parent would strand
-- every child, and the failure would surface two layers away as a resolve error
-- rather than at the rename that caused it.
--
-- ON DELETE RESTRICT, not SET NULL: a parent disappearing out from under a child
-- is exactly what should be refused. SET NULL would silently reparent the child
-- to nothing and quietly drop everything it inherited.
CREATE TABLE IF NOT EXISTS bundles (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT UNIQUE NOT NULL,            -- e.g. "base", "react", "go-service"
    display_name TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    extends_id   INTEGER REFERENCES bundles(id) ON DELETE RESTRICT,  -- parent bundle (inheritance), NULL for a root
    match_tags   TEXT NOT NULL DEFAULT '',        -- JSON array: repo/task tags that select this bundle
    model        TEXT NOT NULL DEFAULT '',        -- optional default model
    effort       TEXT NOT NULL DEFAULT '',        -- optional default effort
    denied_read_paths TEXT NOT NULL DEFAULT '',   -- JSON array: path patterns the session may not read
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_bundles_enabled ON bundles(enabled);

-- The child side of the inheritance foreign key. The chain walk reads parents by
-- primary key, but every DELETE has to look for children before RESTRICT can
-- refuse it, and that lookup is this index.
CREATE INDEX IF NOT EXISTS idx_bundles_extends ON bundles(extends_id);

-- bundle_members: the skills/tools a bundle contributes. `kind` selects which
-- store assigned `member_id` — skill-store for 'skill', tool-store for 'tool'.
-- `condition`, if set, is a task tag that gates inclusion — the member is only
-- added when the resolve request's task_tags contain it (e.g. chrome-devtools
-- only when task=perf).
--
-- `effect` is 'include' (add the member) or 'deny' (take the tool away from the
-- session, even if another bundle includes it or the harness turns it on by
-- default). Only a tool can be denied. The Go list MemberEffects is the source
-- of these values; the CHECKs here are the database refusing anything else,
-- and TestTheSchemaChecksAgreeWithMemberEffects keeps the two in step. The
-- primary key leaves out effect on purpose: one bundle includes a tool or
-- denies it, never both.
--
-- `member_id` is the identity, `name` is a display label kept alongside it and
-- never read back to find a row. Names are not unique across skill-store's
-- sources: 13 SKILL.md names are carried by both the anthropics and antigravity
-- repositories today, so a name join picks whichever row the query happened to
-- reach first. skill-store's only install route is POST /skills/{id}/install,
-- so a member that carries a name and no id is not installable at all.
CREATE TABLE IF NOT EXISTS bundle_members (
    bundle_id  INTEGER NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,                     -- 'skill' | 'tool'
    member_id  INTEGER NOT NULL,                  -- id assigned by skill-store / tool-store
    name       TEXT NOT NULL DEFAULT '',          -- display label, never a lookup key
    condition  TEXT NOT NULL DEFAULT '',          -- optional task-tag gate
    effect     TEXT NOT NULL DEFAULT 'include'    -- 'include' | 'deny'; see above
               CHECK (effect IN ('include', 'deny'))
               CHECK (effect = 'include' OR kind = 'tool'),
    PRIMARY KEY (bundle_id, kind, member_id),
    CHECK (kind IN ('skill', 'tool')),
    CHECK (member_id > 0)
);

CREATE INDEX IF NOT EXISTS idx_bundle_members_bundle ON bundle_members(bundle_id);
