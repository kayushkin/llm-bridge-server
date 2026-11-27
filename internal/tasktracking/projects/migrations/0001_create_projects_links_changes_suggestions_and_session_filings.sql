-- Projects, what each owns in other stores, every change to a project's own
-- fields, the next steps a model suggested, and what the session filer
-- concluded about each chat it judged. Times are Unix seconds, 0 meaning
-- unset, as the API has always served them.
--
-- A project's public id is project_ and its number (FormatRecordID), a
-- suggestion's suggestion_ and its number, a session filing's
-- session_filing_ and its number. Only the numbers are stored, and one record
-- points at another by its number. An identity hands each number out once, so
-- an id written on a card never comes to mean a different project.

CREATE TABLE projects (
    number             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name               text NOT NULL,
    kind               text NOT NULL,                  -- project | stream
    parent_number      bigint REFERENCES projects (number),
    goal               text NOT NULL DEFAULT '',
    done_when          text NOT NULL DEFAULT '',
    stage              text NOT NULL,
    -- The share of all spend the user means this project to take, 0 to 1;
    -- NULL when nobody set one.
    spend_target_share double precision,
    created_by         text NOT NULL DEFAULT '',
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    archived_at        bigint NOT NULL DEFAULT 0
);
CREATE INDEX projects_parent_index ON projects (parent_number);

-- What a project owns, each thing named by the id its own store gave it.
-- label is the owner's name for it when linked, for display only.
CREATE TABLE project_links (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_number bigint NOT NULL REFERENCES projects (number),
    entity_type    text NOT NULL,
    entity_ref     text NOT NULL,
    label          text NOT NULL DEFAULT '',
    note           text NOT NULL DEFAULT '',
    created_by     text NOT NULL DEFAULT '',
    created_at     bigint NOT NULL,
    UNIQUE (project_number, entity_type, entity_ref)
);
CREATE INDEX project_links_entity_index ON project_links (entity_type, entity_ref);

-- Every change to a project's own fields, oldest first: how its stage moved
-- and who moved it.
CREATE TABLE project_changes (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_number bigint NOT NULL REFERENCES projects (number),
    field          text NOT NULL,
    old_value      text NOT NULL,
    new_value      text NOT NULL,
    changed_by     text NOT NULL DEFAULT '',
    changed_at     bigint NOT NULL
);
CREATE INDEX project_changes_project_index ON project_changes (project_number, id);

-- Next steps a model proposed for a project, kept until a person accepts one
-- (it becomes a noteboard todo linked to the project, note_id) or dismisses
-- it. A later round of suggestions supersedes the proposals still undecided.
CREATE TABLE suggestions (
    number         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_number bigint NOT NULL REFERENCES projects (number),
    title          text NOT NULL,
    why            text NOT NULL DEFAULT '',
    status         text NOT NULL,                  -- proposed | accepted | dismissed | superseded
    model          text NOT NULL DEFAULT '',       -- the model that answered
    requested_by   text NOT NULL DEFAULT '',
    created_at     bigint NOT NULL,
    decided_by     text NOT NULL DEFAULT '',
    decided_at     bigint NOT NULL DEFAULT 0,
    note_id        text NOT NULL DEFAULT ''        -- noteboard todo made on accept
);
CREATE INDEX suggestions_project_index ON suggestions (project_number, status, number);

-- What the session filer concluded about one chat session, each time it
-- judged it: filed it under a project by itself, proposed a project (or none)
-- for a person to decide, or found nothing to file. A later judgment of the
-- same session is a new row, and the proposal before it that nobody decided
-- on becomes superseded.
--
-- link_id is the project_links row a filed or accepted filing made or found,
-- 0 for none. It has no foreign key: the link may be deleted later, and the
-- filing still records what it did.
CREATE TABLE session_filings (
    number               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id           text NOT NULL,                 -- llm-bridge-server's id, br_…
    session_label        text NOT NULL DEFAULT '',      -- the session's name when judged, for display only
    evidence             text NOT NULL,                 -- brief | name | none
    goal                 text NOT NULL DEFAULT '',      -- the brief's overall goal as judged; '' without a brief
    repo_store_ids       jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(repo_store_ids) = 'array'),
    project_number       bigint REFERENCES projects (number),       -- the project picked; NULL when none fits
    confidence           double precision NOT NULL DEFAULT 0,
    why                  text NOT NULL DEFAULT '',
    model                text NOT NULL DEFAULT '',      -- '' when no model was asked
    status               text NOT NULL,                 -- filed | proposed | accepted | dismissed | superseded | nothing_to_file
    link_id              bigint NOT NULL DEFAULT 0,
    filed_project_number bigint REFERENCES projects (number),       -- the project of link_id
    created_by           text NOT NULL DEFAULT '',
    created_at           bigint NOT NULL,
    decided_by           text NOT NULL DEFAULT '',
    decided_at           bigint NOT NULL DEFAULT 0
);
CREATE INDEX session_filings_session_index ON session_filings (session_id, number);
CREATE INDEX session_filings_status_index  ON session_filings (status, number);
