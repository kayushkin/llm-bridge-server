-- The rule engine's two tables: the rules a tool call is judged by, and one
-- audit row for every verdict. Ids and times keep the text forms the
-- standalone store gave them: a rule's id is chosen by whoever writes it
-- (seed-v1-…, multichat-send-message-ask) or minted as rule_<nanoseconds>_<hex>,
-- an audit row's is aud_<nanoseconds>_<hex>, and times are RFC 3339 with an
-- offset.

-- A rule is (scope, tool, RE2 pattern) → allow | deny | ask. It has no
-- subject: it says which tool, never for whom. scope is global,
-- bridge:<session id> or instance:<id>.
CREATE TABLE rules (
    id         text PRIMARY KEY,
    scope      text NOT NULL,
    priority   integer NOT NULL DEFAULT 100,
    tool       text NOT NULL,
    pattern    text NOT NULL DEFAULT '',
    outcome    text NOT NULL,
    message    text NOT NULL DEFAULT '',
    enabled    boolean NOT NULL DEFAULT true,
    created_at text NOT NULL,
    updated_at text NOT NULL
);
CREATE INDEX rules_scope_priority_index ON rules (scope, priority);
CREATE INDEX rules_tool_index ON rules (tool);

-- Kept forever. number is the order rows were written in, which breaks a tie
-- between two rows with the same ts the way the standalone store's row order
-- did; it is never shown.
CREATE TABLE audit (
    number          bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    id              text PRIMARY KEY,
    ts              text NOT NULL,
    bridge_id       text NOT NULL DEFAULT '',
    instance_id     text NOT NULL DEFAULT '',
    tool            text NOT NULL,
    input_json      text NOT NULL DEFAULT '',
    atoms_json      text NOT NULL DEFAULT '',
    outcome         text NOT NULL,
    matched_rule_id text NOT NULL DEFAULT '',
    resolved_by     text NOT NULL DEFAULT ''
);
CREATE INDEX audit_ts_index ON audit (ts COLLATE "C", number);
CREATE INDEX audit_bridge_index ON audit (bridge_id);
CREATE INDEX audit_instance_index ON audit (instance_id);
