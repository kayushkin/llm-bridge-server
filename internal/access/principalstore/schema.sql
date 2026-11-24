-- principal-store schema. Create-only and idempotent: Open() executes this
-- whole file on every boot. New columns go in the ensureColumns loop in
-- store.go, never in an edit to a CREATE TABLE below — an edited CREATE TABLE
-- is a no-op against a database that already exists.

PRAGMA foreign_keys = ON;

-- A principal is something a permission can be granted to and a card can be
-- assigned to: a human or a group. This is the row other stores join on, so
-- there is NO hard delete anywhere in this file or in the Go code — a row that
-- vanished would leave dangling assignee ids in kanban and dangling grantees in
-- permission-store. `disabled_at` is the only removal.
CREATE TABLE IF NOT EXISTS principals (
    id            TEXT PRIMARY KEY,            -- principal_000001; see formatID
    seq           INTEGER NOT NULL UNIQUE,     -- monotonic; generates id, doubles as the FTS rowid
    kind          TEXT NOT NULL,               -- 'human' | 'group'; enforced in Go against kind.go
    display_name  TEXT NOT NULL,
    email         TEXT NOT NULL DEFAULT '',    -- humans; NOT unique, never a join key
    disabled_at   INTEGER NOT NULL DEFAULT 0,  -- 0 = active
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_principals_kind     ON principals(kind, disabled_at);
CREATE INDEX IF NOT EXISTS idx_principals_disabled ON principals(disabled_at);

-- Who is in which group. Both sides are principal ids; the Go layer enforces
-- that group_id is a 'group' row and member_id a 'human' row (no nested groups
-- in v1), because a CHECK cannot look across rows.
CREATE TABLE IF NOT EXISTS group_members (
    group_id   TEXT NOT NULL REFERENCES principals(id),
    member_id  TEXT NOT NULL REFERENCES principals(id),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (group_id, member_id)
);
CREATE INDEX IF NOT EXISTS idx_group_members_member ON group_members(member_id);

-- An absence: a half-open [starts_at, ends_at) in epoch seconds, beside the
-- declared week (principals.availability, added in store.go's ensureColumns)
-- rather than editing it, so a holiday is one row and not a rewrite of the
-- schedule. Overlaps are allowed. Only a human has rows here; enforced in Go.
-- This IS hard-deleted: nothing joins on a time_off id.
CREATE TABLE IF NOT EXISTS time_off (
    id           TEXT PRIMARY KEY,            -- timeoff_000001
    seq          INTEGER NOT NULL UNIQUE,
    principal_id TEXT NOT NULL REFERENCES principals(id),
    starts_at    INTEGER NOT NULL,
    ends_at      INTEGER NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_time_off_principal ON time_off(principal_id, starts_at);

-- A sign-in identity linked to one human principal: (provider, tenant_id,
-- subject) is the identity a company login proves, never an email. Written by
-- an administrator, never by the login itself. No row is deleted: unlinked_at
-- is the only removal, and linked_by / unlinked_by / unlink_reason are the
-- audit. See identity_binding.go.
CREATE TABLE IF NOT EXISTS identity_bindings (
    id            TEXT PRIMARY KEY,            -- identity_binding_000001
    seq           INTEGER NOT NULL UNIQUE,
    principal_id  TEXT NOT NULL REFERENCES principals(id),
    provider      TEXT NOT NULL,               -- 'entra'; enforced in Go
    tenant_id     TEXT NOT NULL,               -- lowercased
    subject       TEXT NOT NULL,               -- lowercased; Entra's oid
    linked_at     INTEGER NOT NULL,
    linked_by     TEXT NOT NULL REFERENCES principals(id),
    link_note     TEXT NOT NULL DEFAULT '',
    unlinked_at   INTEGER NOT NULL DEFAULT 0,  -- 0 = active
    unlinked_by   TEXT NOT NULL DEFAULT '',
    unlink_reason TEXT NOT NULL DEFAULT ''
);
-- One active binding per identity. The Go check names the existing row; this
-- index is what holds when two links race.
CREATE UNIQUE INDEX IF NOT EXISTS idx_identity_bindings_active
    ON identity_bindings(provider, tenant_id, subject) WHERE unlinked_at = 0;
CREATE INDEX IF NOT EXISTS idx_identity_bindings_principal ON identity_bindings(principal_id);

-- principal_resources — "what a principal works with" — lived here from
-- 2026-09-10 to 2026-09-11 and moved to grant-store (:8315) as the advisory
-- works_with relation, beside the enforced grants, so there is one place that
-- says who may use what. The table held no rows on any live database when it
-- went (measured on this host: 0 rows on both days), so this drop is the
-- whole migration. Kept as a DROP rather than deleted from the file so a
-- database that predates the move is cleaned up on its next boot.
DROP TABLE IF EXISTS principal_resources;

-- Content-carrying (not external-content) so the triggers below can stay simple
-- deletes and inserts rather than fts5 'delete' commands. rowid is `seq`.
CREATE VIRTUAL TABLE IF NOT EXISTS principals_fts USING fts5(
    display_name, email
);

CREATE TRIGGER IF NOT EXISTS principals_fts_after_insert AFTER INSERT ON principals BEGIN
    INSERT INTO principals_fts(rowid, display_name, email)
    VALUES (new.seq, new.display_name, new.email);
END;

CREATE TRIGGER IF NOT EXISTS principals_fts_after_delete AFTER DELETE ON principals BEGIN
    DELETE FROM principals_fts WHERE rowid = old.seq;
END;

CREATE TRIGGER IF NOT EXISTS principals_fts_after_update AFTER UPDATE ON principals BEGIN
    DELETE FROM principals_fts WHERE rowid = old.seq;
    INSERT INTO principals_fts(rowid, display_name, email)
    VALUES (new.seq, new.display_name, new.email);
END;
