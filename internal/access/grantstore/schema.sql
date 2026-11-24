-- grant-store schema. Create-only and idempotent: Open() executes this whole
-- file on every boot. New columns go in the ensureColumns loop in store.go,
-- never in an edit to a CREATE TABLE below — an edited CREATE TABLE is a no-op
-- against a database that already exists.

PRAGMA foreign_keys = ON;

-- A grant is one relationship tuple: this principal (a human or a group from
-- principal-store) stands in this relation to this resource. "Who may use
-- what" lives here and nowhere else; principal-store keeps the directory and
-- permission-store keeps the call-time pattern rules.
--
-- Neither side is a foreign key: both ids belong to other databases. The Go
-- layer asks principal-store whether the principal exists and the resource's
-- owner whether the resource exists, on every write, so no row ever names an
-- id nobody hands out.
--
-- There is no hard delete. A grant is revoked (revoked_at set), never removed,
-- so a session that was offered a tool can always point at the grant that
-- offered it. The partial unique index below is what makes "grant the same
-- thing twice" idempotent while still allowing a revoked row and a fresh
-- active row for the same tuple to coexist.
CREATE TABLE IF NOT EXISTS grants (
    id            TEXT PRIMARY KEY,               -- grant_000001; see formatID
    seq           INTEGER NOT NULL UNIQUE,        -- monotonic; generates id
    principal_id  TEXT NOT NULL,                  -- principal-store id; checked on write, not a FK
    relation      TEXT NOT NULL,                  -- relation.go; enforced in Go
    resource_type TEXT NOT NULL,                  -- resource_type.go; enforced in Go
    resource_id   TEXT NOT NULL,                  -- the owner's id as text
    note          TEXT NOT NULL DEFAULT '',       -- why, for the person reading the list later
    granted_at    INTEGER NOT NULL,
    revoked_at    INTEGER NOT NULL DEFAULT 0      -- 0 = active
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_grants_active_tuple
    ON grants(principal_id, relation, resource_type, resource_id) WHERE revoked_at = 0;
CREATE INDEX IF NOT EXISTS idx_grants_principal ON grants(principal_id, revoked_at);
-- The reverse lookup: who holds a grant on this resource.
CREATE INDEX IF NOT EXISTS idx_grants_resource  ON grants(resource_type, resource_id, revoked_at);
