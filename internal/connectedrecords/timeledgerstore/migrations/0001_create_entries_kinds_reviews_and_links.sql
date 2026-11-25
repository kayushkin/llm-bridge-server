-- time-ledger-store: an account of what someone did, when and where.
--
-- A time entry is one thing done (or planned and skipped) over a span of time.
-- Entries overlap freely and nest: a child lies inside its parent's span.
-- What brought an entry in and what shows whether it happened are links, one
-- table per kind of record linked, each by the id the owning store gave it.
-- Reviews say whether the entry happened; the newest operator review stands,
-- else the newest model review, else the outcome is unknown. Times are epoch
-- seconds.
--
-- A record's public id is its type's prefix and its number (FormatRecordID):
-- time_entry_000001, time_entry_kind_000001, time_entry_review_000001. Only
-- the number is stored.
--
-- Every row carries owner_principal_id, the principal-store id of the person
-- whose account it is. A child row carries its parent's owner, and its
-- foreign key names both, so an entry's parent, kinds, reviews and links are
-- always the same owner's.

CREATE TABLE time_entries (
    seq                  bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text    NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    title                text    NOT NULL,
    title_by             text    NOT NULL,
    title_by_kind        text    NOT NULL CHECK (title_by_kind IN ('operator', 'model', 'program')),
    span_by              text    NOT NULL,
    span_by_kind         text    NOT NULL CHECK (span_by_kind IN ('operator', 'model', 'program')),
    starts_at            bigint  NOT NULL,
    ends_at              bigint  NOT NULL,
    all_day              boolean NOT NULL DEFAULT false,
    start_date           text    NOT NULL DEFAULT '',
    end_date             text    NOT NULL DEFAULT '',
    time_zone            text    NOT NULL,
    place                text    NOT NULL DEFAULT '',
    place_by             text    NOT NULL DEFAULT '',
    place_by_kind        text    NOT NULL DEFAULT '' CHECK (place_by_kind IN ('', 'operator', 'model', 'program')),
    -- The entry this one lies inside, in the same owner's account; NULL for none.
    parent_seq           bigint,
    parent_set_by        text    NOT NULL DEFAULT '',
    parent_set_by_kind   text    NOT NULL DEFAULT '' CHECK (parent_set_by_kind IN ('', 'operator', 'model', 'program')),
    created_by           text    NOT NULL,
    created_by_kind      text    NOT NULL CHECK (created_by_kind IN ('operator', 'model', 'program')),
    created_reason       text    NOT NULL,
    created_at           bigint  NOT NULL,
    updated_at           bigint  NOT NULL,
    -- When the entry's span or any of its links last changed: a review older
    -- than this has not seen everything there is.
    evidence_changed_at  bigint  NOT NULL,
    deleted_at           bigint,
    deleted_by           text    NOT NULL DEFAULT '',
    CHECK (ends_at >= starts_at),
    UNIQUE (owner_principal_id, seq),
    -- Deferrable so an import can copy a child before a parent made after it;
    -- every other write checks it at once.
    CONSTRAINT time_entries_parent_is_the_owners FOREIGN KEY (owner_principal_id, parent_seq)
        REFERENCES time_entries (owner_principal_id, seq) DEFERRABLE INITIALLY IMMEDIATE
);
CREATE INDEX time_entries_owner_span ON time_entries (owner_principal_id, starts_at, seq);
CREATE INDEX time_entries_span ON time_entries (starts_at, ends_at);
CREATE INDEX time_entries_parent ON time_entries (parent_seq);

-- An entry as it was before each change, as JSON text.
CREATE TABLE time_entry_revisions (
    revision_id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq     bigint NOT NULL,
    reason             text   NOT NULL,
    replaced_at        bigint NOT NULL,
    entry              text   NOT NULL,
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE INDEX time_entry_revisions_entry ON time_entry_revisions (time_entry_seq);

-- The labels an owner's entries carry. A name is unique within one owner's
-- account, so two people may each keep a kind called work.
CREATE TABLE time_entry_kinds (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    name               text   NOT NULL,
    description        text   NOT NULL,
    created_by         text   NOT NULL,
    created_by_kind    text   NOT NULL CHECK (created_by_kind IN ('operator', 'model', 'program')),
    created_at         bigint NOT NULL,
    UNIQUE (owner_principal_id, name),
    UNIQUE (owner_principal_id, seq)
);

CREATE TABLE time_entry_kind_assignments (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq     bigint NOT NULL,
    kind_seq           bigint NOT NULL,
    assigned_by        text   NOT NULL,
    assigned_by_kind   text   NOT NULL CHECK (assigned_by_kind IN ('operator', 'model', 'program')),
    reason             text   NOT NULL,
    assigned_at        bigint NOT NULL,
    removed_at         bigint,
    removed_by         text   NOT NULL DEFAULT '',
    removed_by_kind    text   NOT NULL DEFAULT '' CHECK (removed_by_kind IN ('', 'operator', 'model', 'program')),
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, kind_seq) REFERENCES time_entry_kinds (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_kind_assignments_live
    ON time_entry_kind_assignments (time_entry_seq, kind_seq) WHERE removed_at IS NULL;

CREATE TABLE time_entry_reviews (
    seq                            bigint           GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id             text             NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq                 bigint           NOT NULL,
    outcome                        text             NOT NULL CHECK (outcome IN ('done', 'skipped', 'unknown')),
    reason                         text             NOT NULL,
    reviewed_by                    text             NOT NULL,
    reviewed_by_kind               text             NOT NULL CHECK (reviewed_by_kind IN ('operator', 'model')),
    model                          text             NOT NULL DEFAULT '',
    input_tokens                   bigint           NOT NULL DEFAULT 0,
    output_tokens                  bigint           NOT NULL DEFAULT 0,
    cost_usd                       double precision NOT NULL DEFAULT 0,
    -- JSON text, kept byte for byte as the reviewer sent it.
    links_seen                     text             NOT NULL DEFAULT '[]',
    suggested_parent_time_entry_id text             NOT NULL DEFAULT '',
    suggested_kind_names           text             NOT NULL DEFAULT '[]',
    created_at                     bigint           NOT NULL,
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE INDEX time_entry_reviews_entry ON time_entry_reviews (time_entry_seq, seq);

-- Links. One table per kind of record, each keyed by the owning store's id
-- (links.go's LinkTypes names them). bears_on says what the link is to the
-- entry: the record that brought it in (source), a sign it happened (done) or
-- did not (skipped), money spent on it (cost), or something a reviewer should
-- see that points neither way (context). A live source belongs to one entry
-- of its owner's only. A link the operator removed is never made again by a
-- model or a program.

CREATE TABLE time_entry_calendar_events (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    calendar_event_id    text   NOT NULL,
    occurrence_starts_at bigint NOT NULL,
    bears_on             text   NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_calendar_events_live
    ON time_entry_calendar_events (time_entry_seq, calendar_event_id, occurrence_starts_at) WHERE removed_at IS NULL;
CREATE INDEX time_entry_calendar_events_ref ON time_entry_calendar_events (owner_principal_id, calendar_event_id);
CREATE UNIQUE INDEX time_entry_calendar_events_one_source
    ON time_entry_calendar_events (owner_principal_id, calendar_event_id, occurrence_starts_at) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE time_entry_discord_events (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    discord_event_id     text   NOT NULL,
    occurrence_starts_at bigint NOT NULL,
    bears_on             text   NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_discord_events_live
    ON time_entry_discord_events (time_entry_seq, discord_event_id, occurrence_starts_at) WHERE removed_at IS NULL;
CREATE INDEX time_entry_discord_events_ref ON time_entry_discord_events (owner_principal_id, discord_event_id);
CREATE UNIQUE INDEX time_entry_discord_events_one_source
    ON time_entry_discord_events (owner_principal_id, discord_event_id, occurrence_starts_at) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE time_entry_event_store_events (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    event_store_event_id text   NOT NULL,
    bears_on             text   NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_event_store_events_live
    ON time_entry_event_store_events (time_entry_seq, event_store_event_id) WHERE removed_at IS NULL;
CREATE INDEX time_entry_event_store_events_ref ON time_entry_event_store_events (owner_principal_id, event_store_event_id);
CREATE UNIQUE INDEX time_entry_event_store_events_one_source
    ON time_entry_event_store_events (owner_principal_id, event_store_event_id) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE time_entry_chat_messages (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    chat_message_id      text   NOT NULL,
    bears_on             text   NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_chat_messages_live
    ON time_entry_chat_messages (time_entry_seq, chat_message_id) WHERE removed_at IS NULL;
CREATE INDEX time_entry_chat_messages_ref ON time_entry_chat_messages (owner_principal_id, chat_message_id);
CREATE UNIQUE INDEX time_entry_chat_messages_one_source
    ON time_entry_chat_messages (owner_principal_id, chat_message_id) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE time_entry_trips (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    trip_id              text   NOT NULL,
    overlap_seconds      bigint NOT NULL DEFAULT 0,
    bears_on             text   NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_trips_live
    ON time_entry_trips (time_entry_seq, trip_id) WHERE removed_at IS NULL;
CREATE INDEX time_entry_trips_ref ON time_entry_trips (owner_principal_id, trip_id);
CREATE UNIQUE INDEX time_entry_trips_one_source
    ON time_entry_trips (owner_principal_id, trip_id) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE time_entry_visits (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    visit_id             text   NOT NULL,
    overlap_seconds      bigint NOT NULL DEFAULT 0,
    bears_on             text   NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_visits_live
    ON time_entry_visits (time_entry_seq, visit_id) WHERE removed_at IS NULL;
CREATE INDEX time_entry_visits_ref ON time_entry_visits (owner_principal_id, visit_id);

CREATE TABLE time_entry_photos (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    photo_id             text   NOT NULL,
    bears_on             text   NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_photos_live
    ON time_entry_photos (time_entry_seq, photo_id) WHERE removed_at IS NULL;
CREATE INDEX time_entry_photos_ref ON time_entry_photos (owner_principal_id, photo_id);

CREATE TABLE time_entry_transactions (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    transaction_id       text   NOT NULL,
    amount_cents         bigint,
    bears_on             text   NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_transactions_live
    ON time_entry_transactions (time_entry_seq, transaction_id) WHERE removed_at IS NULL;
CREATE INDEX time_entry_transactions_ref ON time_entry_transactions (owner_principal_id, transaction_id);

CREATE TABLE time_entry_discord_attendances (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    discord_attendance_id text   NOT NULL,
    bears_on             text   NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_discord_attendances_live
    ON time_entry_discord_attendances (time_entry_seq, discord_attendance_id) WHERE removed_at IS NULL;
CREATE INDEX time_entry_discord_attendances_ref ON time_entry_discord_attendances (owner_principal_id, discord_attendance_id);

CREATE TABLE time_entry_activities (
    seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id   text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    time_entry_seq       bigint NOT NULL,
    activity_id          text   NOT NULL,
    bears_on             text   NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by            text   NOT NULL,
    linked_by_kind       text   NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               text   NOT NULL,
    linked_at            bigint NOT NULL,
    removed_at           bigint,
    removed_by           text   NOT NULL DEFAULT '',
    removed_by_kind      text   NOT NULL DEFAULT '',
    FOREIGN KEY (owner_principal_id, time_entry_seq) REFERENCES time_entries (owner_principal_id, seq)
);
CREATE UNIQUE INDEX time_entry_activities_live
    ON time_entry_activities (time_entry_seq, activity_id) WHERE removed_at IS NULL;
CREATE INDEX time_entry_activities_ref ON time_entry_activities (owner_principal_id, activity_id);
