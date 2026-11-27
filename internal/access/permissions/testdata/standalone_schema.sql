-- The schema the standalone permission-store kept in SQLite (its migrations v1
-- and v2), copied here for the importer test. It is not this store's schema:
-- migrations/ is.
CREATE TABLE schema_version (version INTEGER PRIMARY KEY);
INSERT INTO schema_version (version) VALUES (1), (2);
CREATE TABLE rules (
    id          TEXT PRIMARY KEY,
    scope       TEXT NOT NULL,
    priority    INTEGER NOT NULL DEFAULT 100,
    tool        TEXT NOT NULL,
    pattern     TEXT NOT NULL DEFAULT '',
    outcome     TEXT NOT NULL,
    message     TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
CREATE INDEX idx_rules_scope_priority ON rules(scope, priority);
CREATE INDEX idx_rules_tool ON rules(tool);
CREATE TABLE audit (
    id              TEXT PRIMARY KEY,
    ts              TEXT NOT NULL,
    bridge_id       TEXT NOT NULL DEFAULT '',
    instance_id     TEXT NOT NULL DEFAULT '',
    tool            TEXT NOT NULL,
    input_json      TEXT NOT NULL DEFAULT '',
    atoms_json      TEXT NOT NULL DEFAULT '',
    outcome         TEXT NOT NULL,
    matched_rule_id TEXT NOT NULL DEFAULT '',
    resolved_by     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_ts ON audit(ts);
CREATE INDEX idx_audit_bridge ON audit(bridge_id);
CREATE INDEX idx_audit_instance ON audit(instance_id);
