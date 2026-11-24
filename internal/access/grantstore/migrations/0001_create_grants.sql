-- A grant is one relationship tuple: this principal (a human, a group, a peer
-- or a service from principal-store) stands in this relation to this resource.
-- "Who may use what" lives here and nowhere else.
--
-- Neither side is a foreign key: both ids belong to other modules. The Go
-- layer asks principal-store whether the principal exists and the resource's
-- owner whether the resource exists, on every write, so no row ever names an
-- id nobody hands out.
--
-- There is no hard delete. A grant is revoked (revoked_at set), never removed,
-- so a session that was offered a tool can always point at the grant that
-- offered it. The partial unique index below makes "grant the same thing
-- twice" idempotent while still letting a revoked row and a fresh active row
-- for the same tuple sit side by side.
--
-- A grant's public id is grant_ and its number (FormatRecordID), so the number
-- is the only id stored. Times are Unix seconds, 0 meaning unset.
CREATE TABLE grants (
    seq           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    principal_id  text NOT NULL,
    relation      text NOT NULL,
    resource_type text NOT NULL,
    resource_id   text NOT NULL,
    note          text NOT NULL DEFAULT '',
    granted_at    bigint NOT NULL,
    revoked_at    bigint NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX grants_active_tuple_index
    ON grants (principal_id, relation, resource_type, resource_id) WHERE revoked_at = 0;
CREATE INDEX grants_principal_index ON grants (principal_id, revoked_at);
-- The reverse lookup: who holds a grant on this resource.
CREATE INDEX grants_resource_index ON grants (resource_type, resource_id, revoked_at);
