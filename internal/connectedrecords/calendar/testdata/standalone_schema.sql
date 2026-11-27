-- The live schema of the standalone calendar-store's SQLite file: its
-- schema.sql, plus what its migrate() added (the columns below are already in
-- schema.sql; the index is migrate()'s). The import test builds a file from it.


CREATE TABLE IF NOT EXISTS calendars (
    seq        INTEGER PRIMARY KEY,
    id         TEXT    NOT NULL UNIQUE,
    name       TEXT    NOT NULL,
    -- source names who keeps the calendar (google); source_ref is that
    -- source's own id for it. Together they find the calendar again.
    source     TEXT    NOT NULL,
    source_ref TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (source, source_ref)
);

CREATE TABLE IF NOT EXISTS calendar_events (
    seq                INTEGER PRIMARY KEY,
    id                 TEXT    NOT NULL UNIQUE,
    calendar_id        TEXT    NOT NULL REFERENCES calendars(id),
    -- uid and recurrence_id are the source's key for the event. recurrence_id
    -- is '' for a single event or a series, and the original start of the one
    -- date a changed copy replaces, as the source wrote it.
    uid                TEXT    NOT NULL,
    recurrence_id      TEXT    NOT NULL DEFAULT '',
    -- source_event_id is Google's own id for the event, set once a Google
    -- import has seen it; '' for an event only an .ics file brought. The index
    -- on it is made in migrate(), since older databases gain the column there.
    source_event_id    TEXT    NOT NULL DEFAULT '',
    summary            TEXT    NOT NULL DEFAULT '',
    description        TEXT    NOT NULL DEFAULT '',
    location           TEXT    NOT NULL DEFAULT '',
    -- starts_at and ends_at are epoch seconds. An all-day event has all_day 1,
    -- its dates in start_date and end_date (end exclusive, as iCalendar has
    -- it), and starts_at/ends_at at UTC midnight of those dates, for ordering.
    starts_at          INTEGER NOT NULL,
    ends_at            INTEGER NOT NULL DEFAULT 0,
    all_day            INTEGER NOT NULL DEFAULT 0,
    start_date         TEXT    NOT NULL DEFAULT '',
    end_date           TEXT    NOT NULL DEFAULT '',
    time_zone          TEXT    NOT NULL DEFAULT '',
    recurrence_rule    TEXT    NOT NULL DEFAULT '',
    status             TEXT    NOT NULL DEFAULT '',
    transparency       TEXT    NOT NULL DEFAULT '',
    organizer_email    TEXT    NOT NULL DEFAULT '',
    organizer_name     TEXT    NOT NULL DEFAULT '',
    source_created_at  INTEGER NOT NULL DEFAULT 0,
    source_modified_at INTEGER NOT NULL DEFAULT 0,
    sequence           INTEGER NOT NULL DEFAULT 0,
    -- raw is the whole VEVENT as the source wrote it, so nothing the columns
    -- leave out is lost; raw_hash decides whether an import changes the row.
    raw                TEXT    NOT NULL,
    raw_hash           TEXT    NOT NULL,
    import_id          TEXT    NOT NULL,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    UNIQUE (calendar_id, uid, recurrence_id)
);
CREATE INDEX IF NOT EXISTS calendar_events_starts_at ON calendar_events(starts_at);

CREATE TABLE IF NOT EXISTS calendar_event_attendees (
    event_id             TEXT    NOT NULL REFERENCES calendar_events(id),
    position             INTEGER NOT NULL,
    email                TEXT    NOT NULL DEFAULT '',
    name                 TEXT    NOT NULL DEFAULT '',
    role                 TEXT    NOT NULL DEFAULT '',
    participation_status TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (event_id, position)
);

-- Who an event was with, as people-store ids. linked_by says how the link was
-- made (GET /vocabularies); attendee-email links are made by every import
-- from the attendees' addresses and remade when the event changes.
CREATE TABLE IF NOT EXISTS calendar_event_people (
    event_id  TEXT    NOT NULL REFERENCES calendar_events(id),
    person_id TEXT    NOT NULL,
    linked_by TEXT    NOT NULL,
    reason    TEXT    NOT NULL DEFAULT '',
    linked_at INTEGER NOT NULL,
    PRIMARY KEY (event_id, person_id)
);
CREATE INDEX IF NOT EXISTS calendar_event_people_person ON calendar_event_people(person_id);

-- Before an import changes an event, the whole row as it was, as JSON.
CREATE TABLE IF NOT EXISTS calendar_event_revisions (
    seq         INTEGER PRIMARY KEY,
    event_id    TEXT    NOT NULL REFERENCES calendar_events(id),
    reason      TEXT    NOT NULL,
    replaced_at INTEGER NOT NULL,
    event_json  TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS calendar_imports (
    seq         INTEGER PRIMARY KEY,
    id          TEXT    NOT NULL UNIQUE,
    calendar_id TEXT    NOT NULL REFERENCES calendars(id),
    file_name   TEXT    NOT NULL DEFAULT '',
    -- format is how the body was written: ics or google-event-json.
    format      TEXT    NOT NULL DEFAULT 'ics',
    file_sha256 TEXT    NOT NULL,
    created     INTEGER NOT NULL DEFAULT 0,
    updated     INTEGER NOT NULL DEFAULT 0,
    unchanged   INTEGER NOT NULL DEFAULT 0,
    people_linked INTEGER NOT NULL DEFAULT 0,
    -- skipped counts events deleted from Google, sent with only their id,
    -- that the store never held.
    skipped     INTEGER NOT NULL DEFAULT 0,
    imported_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS calendar_events_source_event_id
    ON calendar_events(calendar_id, source_event_id) WHERE source_event_id != '';
