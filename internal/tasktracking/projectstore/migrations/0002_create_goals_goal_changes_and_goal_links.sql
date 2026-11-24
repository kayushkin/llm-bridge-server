-- A goal of a project: something wanted that finishes. A sub-project is a
-- lasting area with its own repos; a goal is reached or dropped. A goal a
-- model or agent suggested is proposed until a person accepts or dismisses it.
-- Its public id is goal_ and its number (FormatRecordID).
CREATE TABLE goals (
    number             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_number     bigint NOT NULL REFERENCES projects (number),
    parent_goal_number bigint REFERENCES goals (number),  -- a goal of the same project; NULL at the top
    statement          text NOT NULL,                     -- one sentence: what is wanted
    done_when          text NOT NULL DEFAULT '',          -- how anyone can tell it is reached
    state              text NOT NULL,                     -- proposed | open | reached | dropped | dismissed
    priority           integer NOT NULL DEFAULT 0,        -- as noteboard's: a higher number is more urgent
    target_at          bigint NOT NULL DEFAULT 0,         -- unix seconds; 0 when no date is set
    created_by         text NOT NULL DEFAULT '',
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    -- Set while the goal is reached, dropped or dismissed; cleared on reopen.
    closed_by          text NOT NULL DEFAULT '',
    closed_at          bigint NOT NULL DEFAULT 0,
    closed_because     text NOT NULL DEFAULT ''
);
CREATE INDEX goals_project_index ON goals (project_number, state, number);

-- Every change to a goal's own fields, oldest first.
CREATE TABLE goal_changes (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    goal_number bigint NOT NULL REFERENCES goals (number),
    field       text NOT NULL,
    old_value   text NOT NULL,
    new_value   text NOT NULL,
    changed_by  text NOT NULL DEFAULT '',
    changed_at  bigint NOT NULL
);
CREATE INDEX goal_changes_goal_index ON goal_changes (goal_number, id);

-- The work that serves a goal, each thing named by the id its own store gave
-- it: a noteboard item (usually a todo) or a session. label is the owner's
-- name for it when linked, for display only.
CREATE TABLE goal_links (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    goal_number bigint NOT NULL REFERENCES goals (number),
    entity_type text NOT NULL,                    -- note | session
    entity_ref  text NOT NULL,
    label       text NOT NULL DEFAULT '',
    note        text NOT NULL DEFAULT '',
    created_by  text NOT NULL DEFAULT '',
    created_at  bigint NOT NULL,
    UNIQUE (goal_number, entity_type, entity_ref)
);
CREATE INDEX goal_links_entity_index ON goal_links (entity_type, entity_ref);
