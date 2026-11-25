-- time-ledger-store: an account of what the operator did, when and where.
--
-- A time entry is one thing done (or planned and skipped) over a span of time.
-- Entries overlap freely and nest: a child lies inside its parent's span.
-- What brought an entry in and what shows whether it happened are links, one
-- table per kind of record linked, each by the id the owning store gave it.
-- Reviews say whether the entry happened; the newest operator review stands,
-- else the newest model review, else the outcome is unknown.

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS time_entries (
    seq                  INTEGER PRIMARY KEY AUTOINCREMENT,
    id                   TEXT NOT NULL UNIQUE,
    title                TEXT NOT NULL,
    title_by             TEXT NOT NULL,
    title_by_kind        TEXT NOT NULL CHECK (title_by_kind IN ('operator', 'model', 'program')),
    span_by              TEXT NOT NULL,
    span_by_kind         TEXT NOT NULL CHECK (span_by_kind IN ('operator', 'model', 'program')),
    starts_at            INTEGER NOT NULL,
    ends_at              INTEGER NOT NULL,
    all_day              INTEGER NOT NULL DEFAULT 0,
    start_date           TEXT NOT NULL DEFAULT '',
    end_date             TEXT NOT NULL DEFAULT '',
    time_zone            TEXT NOT NULL,
    place                TEXT NOT NULL DEFAULT '',
    place_by             TEXT NOT NULL DEFAULT '',
    place_by_kind        TEXT NOT NULL DEFAULT '' CHECK (place_by_kind IN ('', 'operator', 'model', 'program')),
    parent_time_entry_id TEXT REFERENCES time_entries(id),
    parent_set_by        TEXT NOT NULL DEFAULT '',
    parent_set_by_kind   TEXT NOT NULL DEFAULT '' CHECK (parent_set_by_kind IN ('', 'operator', 'model', 'program')),
    created_by           TEXT NOT NULL,
    created_by_kind      TEXT NOT NULL CHECK (created_by_kind IN ('operator', 'model', 'program')),
    created_reason       TEXT NOT NULL,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    -- When the entry's span or any of its links last changed: a review older
    -- than this has not seen everything there is.
    evidence_changed_at  INTEGER NOT NULL,
    deleted_at           INTEGER,
    deleted_by           TEXT NOT NULL DEFAULT '',
    CHECK (ends_at >= starts_at)
);
CREATE INDEX IF NOT EXISTS time_entries_span ON time_entries(starts_at, ends_at);
CREATE INDEX IF NOT EXISTS time_entries_parent ON time_entries(parent_time_entry_id);

CREATE TABLE IF NOT EXISTS time_entry_revisions (
    revision_id   INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id TEXT NOT NULL REFERENCES time_entries(id),
    reason        TEXT NOT NULL,
    replaced_at   INTEGER NOT NULL,
    entry         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS time_entry_revisions_entry ON time_entry_revisions(time_entry_id);

CREATE TABLE IF NOT EXISTS time_entry_kinds (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    id              TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_by_kind TEXT NOT NULL CHECK (created_by_kind IN ('operator', 'model', 'program')),
    created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS time_entry_kind_assignments (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id    TEXT NOT NULL REFERENCES time_entries(id),
    kind_id          TEXT NOT NULL REFERENCES time_entry_kinds(id),
    assigned_by      TEXT NOT NULL,
    assigned_by_kind TEXT NOT NULL CHECK (assigned_by_kind IN ('operator', 'model', 'program')),
    reason           TEXT NOT NULL,
    assigned_at      INTEGER NOT NULL,
    removed_at       INTEGER,
    removed_by       TEXT NOT NULL DEFAULT '',
    removed_by_kind  TEXT NOT NULL DEFAULT '' CHECK (removed_by_kind IN ('', 'operator', 'model', 'program'))
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_kind_assignments_live
    ON time_entry_kind_assignments(time_entry_id, kind_id) WHERE removed_at IS NULL;

CREATE TABLE IF NOT EXISTS time_entry_reviews (
    seq                            INTEGER PRIMARY KEY AUTOINCREMENT,
    id                             TEXT NOT NULL UNIQUE,
    time_entry_id                  TEXT NOT NULL REFERENCES time_entries(id),
    outcome                        TEXT NOT NULL CHECK (outcome IN ('done', 'skipped', 'unknown')),
    reason                         TEXT NOT NULL,
    reviewed_by                    TEXT NOT NULL,
    reviewed_by_kind               TEXT NOT NULL CHECK (reviewed_by_kind IN ('operator', 'model')),
    model                          TEXT NOT NULL DEFAULT '',
    input_tokens                   INTEGER NOT NULL DEFAULT 0,
    output_tokens                  INTEGER NOT NULL DEFAULT 0,
    cost_usd                       REAL NOT NULL DEFAULT 0,
    links_seen                     TEXT NOT NULL DEFAULT '[]',
    suggested_parent_time_entry_id TEXT NOT NULL DEFAULT '',
    suggested_kind_names           TEXT NOT NULL DEFAULT '[]',
    created_at                     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS time_entry_reviews_entry ON time_entry_reviews(time_entry_id, seq);

-- Links. One table per kind of record, each keyed by the owning store's id.
-- bears_on says what the link is to the entry: the record that brought it in
-- (source), a sign it happened (done) or did not (skipped), money spent on it
-- (cost), or something a reviewer should see that points neither way
-- (context). A live source belongs to one entry only. A link the operator
-- removed is never made again by a model or a program.

CREATE TABLE IF NOT EXISTS time_entry_calendar_events (
    seq                  INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id        TEXT NOT NULL REFERENCES time_entries(id),
    calendar_event_id    TEXT NOT NULL,
    occurrence_starts_at INTEGER NOT NULL,
    bears_on             TEXT NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by            TEXT NOT NULL,
    linked_by_kind       TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               TEXT NOT NULL,
    linked_at            INTEGER NOT NULL,
    removed_at           INTEGER,
    removed_by           TEXT NOT NULL DEFAULT '',
    removed_by_kind      TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_calendar_events_live
    ON time_entry_calendar_events(time_entry_id, calendar_event_id, occurrence_starts_at) WHERE removed_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_calendar_events_one_source
    ON time_entry_calendar_events(calendar_event_id, occurrence_starts_at) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE IF NOT EXISTS time_entry_discord_events (
    seq                  INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id        TEXT NOT NULL REFERENCES time_entries(id),
    discord_event_id     TEXT NOT NULL,
    occurrence_starts_at INTEGER NOT NULL,
    bears_on             TEXT NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by            TEXT NOT NULL,
    linked_by_kind       TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               TEXT NOT NULL,
    linked_at            INTEGER NOT NULL,
    removed_at           INTEGER,
    removed_by           TEXT NOT NULL DEFAULT '',
    removed_by_kind      TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_discord_events_live
    ON time_entry_discord_events(time_entry_id, discord_event_id, occurrence_starts_at) WHERE removed_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_discord_events_one_source
    ON time_entry_discord_events(discord_event_id, occurrence_starts_at) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE IF NOT EXISTS time_entry_event_store_events (
    seq                  INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id        TEXT NOT NULL REFERENCES time_entries(id),
    event_store_event_id TEXT NOT NULL,
    bears_on             TEXT NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by            TEXT NOT NULL,
    linked_by_kind       TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason               TEXT NOT NULL,
    linked_at            INTEGER NOT NULL,
    removed_at           INTEGER,
    removed_by           TEXT NOT NULL DEFAULT '',
    removed_by_kind      TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_event_store_events_live
    ON time_entry_event_store_events(time_entry_id, event_store_event_id) WHERE removed_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_event_store_events_one_source
    ON time_entry_event_store_events(event_store_event_id) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE IF NOT EXISTS time_entry_chat_messages (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id   TEXT NOT NULL REFERENCES time_entries(id),
    chat_message_id TEXT NOT NULL,
    bears_on        TEXT NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by       TEXT NOT NULL,
    linked_by_kind  TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason          TEXT NOT NULL,
    linked_at       INTEGER NOT NULL,
    removed_at      INTEGER,
    removed_by      TEXT NOT NULL DEFAULT '',
    removed_by_kind TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_chat_messages_live
    ON time_entry_chat_messages(time_entry_id, chat_message_id) WHERE removed_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_chat_messages_one_source
    ON time_entry_chat_messages(chat_message_id) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE IF NOT EXISTS time_entry_trips (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id   TEXT NOT NULL REFERENCES time_entries(id),
    trip_id         TEXT NOT NULL,
    overlap_seconds INTEGER NOT NULL DEFAULT 0,
    bears_on        TEXT NOT NULL CHECK (bears_on IN ('source', 'done', 'skipped', 'cost', 'context')),
    linked_by       TEXT NOT NULL,
    linked_by_kind  TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason          TEXT NOT NULL,
    linked_at       INTEGER NOT NULL,
    removed_at      INTEGER,
    removed_by      TEXT NOT NULL DEFAULT '',
    removed_by_kind TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_trips_live
    ON time_entry_trips(time_entry_id, trip_id) WHERE removed_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_trips_one_source
    ON time_entry_trips(trip_id) WHERE removed_at IS NULL AND bears_on = 'source';

CREATE TABLE IF NOT EXISTS time_entry_visits (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id   TEXT NOT NULL REFERENCES time_entries(id),
    visit_id        TEXT NOT NULL,
    overlap_seconds INTEGER NOT NULL DEFAULT 0,
    bears_on        TEXT NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by       TEXT NOT NULL,
    linked_by_kind  TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason          TEXT NOT NULL,
    linked_at       INTEGER NOT NULL,
    removed_at      INTEGER,
    removed_by      TEXT NOT NULL DEFAULT '',
    removed_by_kind TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_visits_live
    ON time_entry_visits(time_entry_id, visit_id) WHERE removed_at IS NULL;

CREATE TABLE IF NOT EXISTS time_entry_photos (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id   TEXT NOT NULL REFERENCES time_entries(id),
    photo_id        TEXT NOT NULL,
    bears_on        TEXT NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by       TEXT NOT NULL,
    linked_by_kind  TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason          TEXT NOT NULL,
    linked_at       INTEGER NOT NULL,
    removed_at      INTEGER,
    removed_by      TEXT NOT NULL DEFAULT '',
    removed_by_kind TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_photos_live
    ON time_entry_photos(time_entry_id, photo_id) WHERE removed_at IS NULL;

CREATE TABLE IF NOT EXISTS time_entry_transactions (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id   TEXT NOT NULL REFERENCES time_entries(id),
    transaction_id  TEXT NOT NULL,
    amount_cents    INTEGER,
    bears_on        TEXT NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by       TEXT NOT NULL,
    linked_by_kind  TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason          TEXT NOT NULL,
    linked_at       INTEGER NOT NULL,
    removed_at      INTEGER,
    removed_by      TEXT NOT NULL DEFAULT '',
    removed_by_kind TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_transactions_live
    ON time_entry_transactions(time_entry_id, transaction_id) WHERE removed_at IS NULL;

CREATE TABLE IF NOT EXISTS time_entry_discord_attendances (
    seq                   INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id         TEXT NOT NULL REFERENCES time_entries(id),
    discord_attendance_id TEXT NOT NULL,
    bears_on              TEXT NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by             TEXT NOT NULL,
    linked_by_kind        TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason                TEXT NOT NULL,
    linked_at             INTEGER NOT NULL,
    removed_at            INTEGER,
    removed_by            TEXT NOT NULL DEFAULT '',
    removed_by_kind       TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_discord_attendances_live
    ON time_entry_discord_attendances(time_entry_id, discord_attendance_id) WHERE removed_at IS NULL;

CREATE TABLE IF NOT EXISTS time_entry_activities (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    time_entry_id   TEXT NOT NULL REFERENCES time_entries(id),
    activity_id     TEXT NOT NULL,
    bears_on        TEXT NOT NULL CHECK (bears_on IN ('done', 'skipped', 'cost', 'context')),
    linked_by       TEXT NOT NULL,
    linked_by_kind  TEXT NOT NULL CHECK (linked_by_kind IN ('operator', 'model', 'program')),
    reason          TEXT NOT NULL,
    linked_at       INTEGER NOT NULL,
    removed_at      INTEGER,
    removed_by      TEXT NOT NULL DEFAULT '',
    removed_by_kind TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS time_entry_activities_live
    ON time_entry_activities(time_entry_id, activity_id) WHERE removed_at IS NULL;
