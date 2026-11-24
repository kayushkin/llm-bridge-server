-- Curated "bundles": named, inheritable sets of {skills, tools} (plus optional
-- model/effort defaults) selected for a session by repo + task tags. A bundle
-- is a cross-store join artifact: its members are the ids skill-store and
-- tool-store assigned, carried with the name alongside for display only. This
-- module owns the curated definitions and the resolve composition; it does not
-- own the underlying skills or tools.

-- extends_id is the parent bundle's id, not its name: renaming a parent must
-- not strand its children. ON DELETE RESTRICT, not SET NULL: a parent
-- disappearing from under a child is refused, because SET NULL would make the
-- child a root that inherits nothing while looking healthy.
--
-- match_tags and denied_read_paths are JSON arrays kept as text, '' for none,
-- as the standalone store wrote them.
CREATE TABLE bundles (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name              TEXT UNIQUE NOT NULL,
    display_name      TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    extends_id        BIGINT REFERENCES bundles(id) ON DELETE RESTRICT,
    match_tags        TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    effort            TEXT NOT NULL DEFAULT '',
    denied_read_paths TEXT NOT NULL DEFAULT '',
    enabled           BOOLEAN NOT NULL DEFAULT true,
    created_at        BIGINT NOT NULL,
    updated_at        BIGINT NOT NULL
);

CREATE INDEX bundles_enabled ON bundles(enabled);
-- Every DELETE looks for children before RESTRICT can refuse it.
CREATE INDEX bundles_extends_id ON bundles(extends_id);

-- The skills and tools a bundle contributes. kind selects the store that
-- assigned member_id: skill-store for 'skill', tool-store for 'tool'.
-- condition, if set, is a task tag that gates the member.
--
-- effect is 'include' or 'deny'; only a tool can be denied. The Go list
-- MemberEffects is the source of these values; the CHECKs are the database
-- refusing anything else, and TestTheSchemaChecksAgreeWithMemberEffects keeps
-- the two in step. The primary key leaves effect out: one bundle includes a
-- tool or denies it, never both.
--
-- name is a display label kept beside member_id and never read back to find
-- a row: skill names are not unique across skill-store's sources.
CREATE TABLE bundle_members (
    bundle_id  BIGINT NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    member_id  BIGINT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    condition  TEXT NOT NULL DEFAULT '',
    effect     TEXT NOT NULL DEFAULT 'include',
    PRIMARY KEY (bundle_id, kind, member_id),
    CONSTRAINT bundle_members_kind CHECK (kind IN ('skill', 'tool')),
    CONSTRAINT bundle_members_member_id CHECK (member_id > 0),
    CONSTRAINT bundle_members_effect CHECK (effect IN ('include', 'deny')),
    CONSTRAINT bundle_members_only_a_tool_is_denied CHECK (effect = 'include' OR kind = 'tool')
);

CREATE INDEX bundle_members_bundle_id ON bundle_members(bundle_id);

-- Members of a standalone store written before member_id existed. They carry
-- a name and no id, so nothing resolves them; they are kept so an operator can
-- read them back and re-add them with the ids their owning stores assigned.
-- Nothing in the store reads or writes this table except the import.
CREATE TABLE bundle_members_legacy_name_join (
    bundle_id  BIGINT NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    condition  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (bundle_id, kind, name),
    CHECK (kind IN ('skill', 'tool'))
);
