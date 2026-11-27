-- kanban-store's tables: boards and their columns, where each card sits, what
-- each card is linked to and who is on it, its history, its notes, its
-- ticket row and files, each board's priority ladder, tag rules and message
-- triggers, and board classification.
--
-- This is the schema the standalone SQLite store reached after every one of
-- its own migrations, written once. A card has no row of its own: a card id
-- is a noteboard item id, so card_id is never a foreign key here.
--
-- Every text column sorts and compares byte by byte (COLLATE "C"), as SQLite
-- did, so a list comes back in the order it always did. Times are timestamptz
-- to the microsecond; the store cuts every time it writes to the microsecond,
-- so what a write answers is what a later read answers. The two message tables
-- keep their times as RFC 3339 text, as the standalone store wrote them, and
-- hand them back unchanged. JSON the store hands back as it was written (an
-- event's detail, a taxonomy, a decision's selections) is text, not jsonb, so
-- its key order and spacing never change; a query that looks inside it casts.

-- A board is a view over cards, with the settings its dispatchers and its
-- classifier run with. The settings that hold an object (business_hours,
-- classifier, assignment_pool) are JSON text; NULL means "not set".
CREATE TABLE boards (
    id                   text COLLATE "C" PRIMARY KEY,
    name                 text COLLATE "C" NOT NULL,
    description          text COLLATE "C" DEFAULT '',
    archived             integer NOT NULL DEFAULT 0,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    business_hours       text COLLATE "C",
    default_principal_id text COLLATE "C",
    default_agent_id     text COLLATE "C",
    default_instance_id  text COLLATE "C",
    classifier           text COLLATE "C",
    default_bundle_id    text COLLATE "C",
    assignment_pool      text COLLATE "C",
    organization_id      text COLLATE "C"
);
CREATE INDEX boards_archived_index ON boards (archived);

-- A board's columns, in position order. work_state, lifecycle_state and
-- budget_clock_state are NULL on a column nobody has mapped.
CREATE TABLE columns (
    id                 text COLLATE "C" PRIMARY KEY,
    board_id           text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    name               text COLLATE "C" NOT NULL,
    position           double precision NOT NULL,
    color              text COLLATE "C" DEFAULT '',
    wip_limit          integer,
    auto_status        text COLLATE "C",
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    budget_clock_state text COLLATE "C",
    lifecycle_state    text COLLATE "C",
    work_state         text COLLATE "C"
);
CREATE INDEX columns_board_index ON columns (board_id, position);

-- Where a card sits: one row per card per board.
CREATE TABLE placements (
    card_id    text COLLATE "C" NOT NULL,
    board_id   text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    column_id  text COLLATE "C" NOT NULL REFERENCES columns (id) ON DELETE CASCADE,
    position   double precision NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (card_id, board_id)
);
CREATE INDEX placements_board_index  ON placements (board_id, column_id, position);
CREATE INDEX placements_column_index ON placements (column_id, position);
CREATE INDEX placements_card_index   ON placements (card_id);

-- A card's links to records in other stores, by that store's id.
CREATE TABLE card_links (
    id          text COLLATE "C" PRIMARY KEY,
    card_id     text COLLATE "C" NOT NULL,
    entity_type text COLLATE "C" NOT NULL,
    entity_ref  text COLLATE "C" NOT NULL,
    label       text COLLATE "C" DEFAULT '',
    created_at  timestamptz NOT NULL,
    UNIQUE (card_id, entity_type, entity_ref)
);
CREATE INDEX card_links_card_index   ON card_links (card_id);
CREATE INDEX card_links_entity_index ON card_links (entity_type, entity_ref);

-- Who is on a card.
CREATE TABLE card_assignments (
    card_id      text COLLATE "C" NOT NULL,
    principal_id text COLLATE "C" NOT NULL,
    assigned_by  text COLLATE "C" NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (card_id, principal_id)
);
CREATE INDEX card_assignments_principal_index ON card_assignments (principal_id);

-- Tags on any entity, one tag space shared by every kind.
CREATE TABLE entity_tags (
    entity_type text COLLATE "C" NOT NULL,
    entity_ref  text COLLATE "C" NOT NULL,
    tag         text COLLATE "C" NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (entity_type, entity_ref, tag)
);
CREATE INDEX entity_tags_tag_index    ON entity_tags (tag);
CREATE INDEX entity_tags_entity_index ON entity_tags (entity_type, entity_ref);

-- A card's history, append only. board_id is '' for an event that concerns
-- the card on every board. An event may be corrected by one later event that
-- names it in supersedes_event_id, and by one only: the partial unique index
-- makes a second correction of the same event fail in the database.
CREATE TABLE card_events (
    id                  text COLLATE "C" PRIMARY KEY,
    card_id             text COLLATE "C" NOT NULL,
    board_id            text COLLATE "C" NOT NULL DEFAULT '',
    kind                text COLLATE "C" NOT NULL,
    clock_state         text COLLATE "C" NOT NULL,
    actor               text COLLATE "C" NOT NULL DEFAULT '',
    summary             text COLLATE "C" NOT NULL DEFAULT '',
    from_column_id      text COLLATE "C" NOT NULL DEFAULT '',
    to_column_id        text COLLATE "C" NOT NULL DEFAULT '',
    note_id             text COLLATE "C" NOT NULL DEFAULT '',
    detail              text COLLATE "C" NOT NULL DEFAULT '',
    occurred_at         timestamptz NOT NULL,
    recorded_at         timestamptz NOT NULL,
    supersedes_event_id text COLLATE "C" NOT NULL DEFAULT ''
);
CREATE INDEX card_events_card_index  ON card_events (card_id, occurred_at);
CREATE INDEX card_events_board_index ON card_events (board_id, occurred_at);
CREATE UNIQUE INDEX card_events_supersedes_event_id_index
    ON card_events (supersedes_event_id) WHERE supersedes_event_id <> '';

-- Notes written on a card, and who each is written for.
CREATE TABLE card_notes (
    id         text COLLATE "C" PRIMARY KEY,
    card_id    text COLLATE "C" NOT NULL,
    board_id   text COLLATE "C" NOT NULL DEFAULT '',
    kind       text COLLATE "C" NOT NULL DEFAULT 'note',
    body       text COLLATE "C" NOT NULL,
    actor      text COLLATE "C" NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    visibility text COLLATE "C" NOT NULL DEFAULT 'internal'
);
CREATE INDEX card_notes_card_index ON card_notes (card_id, created_at);

-- A board's priority ladder, one rung per noteboard priority value.
CREATE TABLE board_priority_levels (
    board_id                 text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    priority_value           integer NOT NULL,
    label                    text COLLATE "C" NOT NULL,
    budget_seconds           integer,
    default_auto_hold_at_usd double precision,
    PRIMARY KEY (board_id, priority_value)
);

-- Message triggers and what each firing sent. Their times are RFC 3339 text
-- with nanoseconds, as the standalone store wrote them. A delivery row is the
-- claim on one firing: one per trigger per event.
CREATE TABLE message_triggers (
    id                     text COLLATE "C" PRIMARY KEY,
    board_id               text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    name                   text COLLATE "C" NOT NULL,
    event_kind             text COLLATE "C" NOT NULL,
    to_column_id           text COLLATE "C" NOT NULL DEFAULT '',
    -- NULL means any priority.
    priority_value         integer,
    recipient_user_id      text COLLATE "C" NOT NULL,
    recipient_display_name text COLLATE "C" NOT NULL DEFAULT '',
    message_template       text COLLATE "C" NOT NULL,
    enabled                boolean NOT NULL DEFAULT true,
    created_at             text COLLATE "C" NOT NULL,
    updated_at             text COLLATE "C" NOT NULL
);
CREATE INDEX message_triggers_board_index ON message_triggers (board_id);

CREATE TABLE message_deliveries (
    id                text COLLATE "C" PRIMARY KEY,
    trigger_id        text COLLATE "C" NOT NULL REFERENCES message_triggers (id) ON DELETE CASCADE,
    board_id          text COLLATE "C" NOT NULL,
    event_id          text COLLATE "C" NOT NULL,
    card_id           text COLLATE "C" NOT NULL,
    recipient_user_id text COLLATE "C" NOT NULL,
    rendered_message  text COLLATE "C" NOT NULL DEFAULT '',
    status            text COLLATE "C" NOT NULL,
    error             text COLLATE "C" NOT NULL DEFAULT '',
    room_id           text COLLATE "C" NOT NULL DEFAULT '',
    matrix_event_id   text COLLATE "C" NOT NULL DEFAULT '',
    created_at        text COLLATE "C" NOT NULL,
    UNIQUE (trigger_id, event_id)
);
CREATE INDEX message_deliveries_board_index ON message_deliveries (board_id, created_at DESC);

-- A board's tag rules, in order. tags is a JSON array of strings.
CREATE TABLE board_tag_rules (
    id                   text COLLATE "C" PRIMARY KEY,
    board_id             text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    position             integer NOT NULL,
    tags                 text COLLATE "C" NOT NULL,
    default_principal_id text COLLATE "C",
    default_agent_id     text COLLATE "C",
    default_instance_id  text COLLATE "C",
    default_bundle_id    text COLLATE "C",
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    UNIQUE (board_id, position)
);
CREATE INDEX board_tag_rules_board_index ON board_tag_rules (board_id, position);

-- The ticket row of a card that is a ticket.
CREATE TABLE tickets (
    card_id                text COLLATE "C" PRIMARY KEY,
    requester_principal_id text COLLATE "C" NOT NULL,
    channel                text COLLATE "C" NOT NULL,
    created_at             timestamptz NOT NULL,
    updated_at             timestamptz NOT NULL,
    source_entity_type     text COLLATE "C" NOT NULL DEFAULT '',
    source_entity_ref      text COLLATE "C" NOT NULL DEFAULT ''
);
CREATE INDEX tickets_requester_index ON tickets (requester_principal_id);

-- Which card a file-store file hangs on. file-store keeps the file itself.
CREATE TABLE card_attachments (
    file_id     text COLLATE "C" PRIMARY KEY,
    card_id     text COLLATE "C" NOT NULL,
    visibility  text COLLATE "C" NOT NULL DEFAULT 'internal',
    attached_by text COLLATE "C" NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL
);
CREATE INDEX card_attachments_card_index ON card_attachments (card_id, created_at);

-- Board classification. Every axis and value id the store mints comes from
-- one numbering, shared by both kinds and every board:
-- classification_axis_000001, classification_value_000002. The number is the
-- identity column; an id is never handed out twice.
CREATE TABLE classification_identifiers (
    sequence   bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    id         text COLLATE "C" NOT NULL UNIQUE,
    kind       text COLLATE "C" NOT NULL CHECK (kind IN ('axis', 'value')),
    board_id   text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    axis_id    text COLLATE "C" NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL
);
CREATE INDEX classification_identifiers_board_index ON classification_identifiers (board_id);

-- Each taxonomy a board has had; the newest revision is the one in force. A
-- NULL taxonomy is a revision that cleared it.
CREATE TABLE classification_taxonomy_revisions (
    board_id        text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    revision        bigint NOT NULL,
    taxonomy        text COLLATE "C",
    semantic_digest text COLLATE "C" NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL,
    created_by      text COLLATE "C" NOT NULL,
    PRIMARY KEY (board_id, revision)
);

-- Which id each axis and value of a name-only taxonomy was given when the
-- standalone store first gave them ids.
CREATE TABLE classification_taxonomy_migrations (
    board_id    text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    axis_name   text COLLATE "C" NOT NULL,
    value_name  text COLLATE "C" NOT NULL,
    assigned_id text COLLATE "C" NOT NULL,
    migrated_at timestamptz NOT NULL,
    PRIMARY KEY (board_id, axis_name, value_name)
);

-- Each per-axis policy a board has had; the newest is in force.
CREATE TABLE classification_policy_revisions (
    board_id          text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    revision          bigint NOT NULL,
    axis_policies     text COLLATE "C" NOT NULL,
    taxonomy_revision bigint NOT NULL,
    created_at        timestamptz NOT NULL,
    created_by        text COLLATE "C" NOT NULL,
    PRIMARY KEY (board_id, revision)
);

-- What a classifier published, once per publication key and never changed
-- afterwards, except that purging a card removes the source text it was given.
-- sequence orders a board's decisions and pages them.
CREATE TABLE classification_decisions (
    id                        text COLLATE "C" PRIMARY KEY,
    sequence                  bigint NOT NULL UNIQUE,
    board_id                  text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    organization_id           text COLLATE "C" NOT NULL,
    card_id                   text COLLATE "C" NOT NULL,
    publication_key           text COLLATE "C" NOT NULL UNIQUE,
    publication_digest        text COLLATE "C" NOT NULL,
    operation_id              text COLLATE "C" NOT NULL,
    checkpoint_id             text COLLATE "C" NOT NULL,
    attempt_number            integer NOT NULL,
    initiating_principal_id   text COLLATE "C" NOT NULL,
    source_digest             text COLLATE "C" NOT NULL,
    source_revision           text COLLATE "C" NOT NULL DEFAULT '',
    source_snapshot           text COLLATE "C",
    source_removed_at         timestamptz,
    taxonomy_revision         bigint NOT NULL,
    taxonomy_digest           text COLLATE "C" NOT NULL,
    policy_revision           bigint NOT NULL,
    prompt_revision           text COLLATE "C" NOT NULL,
    model                     text COLLATE "C" NOT NULL,
    origin                    text COLLATE "C" NOT NULL,
    selections                text COLLATE "C" NOT NULL,
    rationale                 text COLLATE "C" NOT NULL DEFAULT '',
    model_reported_confidence double precision,
    needs_review              boolean NOT NULL,
    review_reasons            text COLLATE "C" NOT NULL,
    completed_at              timestamptz NOT NULL,
    published_at              timestamptz NOT NULL,
    FOREIGN KEY (board_id, taxonomy_revision) REFERENCES classification_taxonomy_revisions (board_id, revision),
    FOREIGN KEY (board_id, policy_revision) REFERENCES classification_policy_revisions (board_id, revision)
);
CREATE INDEX classification_decisions_board_index     ON classification_decisions (board_id, sequence);
CREATE INDEX classification_decisions_card_index      ON classification_decisions (board_id, card_id, sequence);
CREATE INDEX classification_decisions_operation_index ON classification_decisions (board_id, operation_id);
CREATE INDEX classification_decisions_source_index    ON classification_decisions (card_id);

-- People's reviews of decisions, append only. The idempotency key is one
-- reviewer's on one board.
CREATE TABLE classification_reviews (
    id                        text COLLATE "C" PRIMARY KEY,
    decision_id               text COLLATE "C" NOT NULL REFERENCES classification_decisions (id) ON DELETE CASCADE,
    board_id                  text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    card_id                   text COLLATE "C" NOT NULL,
    reviewer_principal_id     text COLLATE "C" NOT NULL,
    action                    text COLLATE "C" NOT NULL,
    axis_ids                  text COLLATE "C" NOT NULL,
    corrections               text COLLATE "C",
    explanation               text COLLATE "C" NOT NULL DEFAULT '',
    supersedes_review_ids     text COLLATE "C" NOT NULL,
    expected_labels_revision  bigint NOT NULL,
    resulting_labels_revision bigint NOT NULL,
    taxonomy_revision         bigint NOT NULL,
    policy_revision           bigint NOT NULL,
    idempotency_key           text COLLATE "C" NOT NULL,
    request_digest            text COLLATE "C" NOT NULL,
    created_at                timestamptz NOT NULL,
    UNIQUE (board_id, reviewer_principal_id, idempotency_key)
);
CREATE INDEX classification_reviews_decision_index ON classification_reviews (decision_id, created_at);

-- A card's labels on a board, one row per axis, each naming the review that
-- set it; and the card's labels revision, which moves once per review that
-- changes any of them. value_ids is a JSON array of value ids.
CREATE TABLE classification_labels (
    board_id           text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    card_id            text COLLATE "C" NOT NULL,
    axis_id            text COLLATE "C" NOT NULL,
    value_ids          text COLLATE "C" NOT NULL,
    origin_decision_id text COLLATE "C" NOT NULL,
    origin_review_id   text COLLATE "C" NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (board_id, card_id, axis_id)
);

CREATE TABLE classification_label_revisions (
    board_id   text COLLATE "C" NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    card_id    text COLLATE "C" NOT NULL,
    revision   bigint NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (board_id, card_id)
);
