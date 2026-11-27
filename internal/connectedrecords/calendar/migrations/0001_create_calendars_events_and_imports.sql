-- calendar-store: calendar events as their calendars wrote them. Times are
-- epoch seconds, 0 meaning unset.
--
-- A record's public id is its type's prefix and its number (FormatRecordID):
-- calendar_000001, calendar_event_000001, calendar_import_000001. Only the
-- number is stored.
--
-- Every row carries owner_principal_id, the principal-store id of the person
-- who keeps it. A child row carries its parent's owner, and its foreign key
-- names both, so a child can never hang on another owner's parent.

CREATE TABLE calendars (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    name               text   NOT NULL,
    -- source names who keeps the calendar (google); source_ref is that
    -- source's own id for it. Together they find one owner's calendar again.
    source             text   NOT NULL,
    source_ref         text   NOT NULL,
    created_at         bigint NOT NULL,
    UNIQUE (owner_principal_id, source, source_ref),
    UNIQUE (owner_principal_id, seq)
);

-- An import is written first, with its counts filled in when it ends, so the
-- events it writes can name it.
CREATE TABLE calendar_imports (
    seq                bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text    NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    calendar_seq       bigint  NOT NULL,
    file_name          text    NOT NULL DEFAULT '',
    -- format is how the body was written: ics or google-event-json.
    format             text    NOT NULL DEFAULT 'ics',
    file_sha256        text    NOT NULL,
    created            integer NOT NULL DEFAULT 0,
    updated            integer NOT NULL DEFAULT 0,
    unchanged          integer NOT NULL DEFAULT 0,
    people_linked      integer NOT NULL DEFAULT 0,
    -- skipped counts events deleted from Google, sent with only their id,
    -- that the store never held.
    skipped            integer NOT NULL DEFAULT 0,
    imported_at        bigint  NOT NULL,
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, calendar_seq) REFERENCES calendars (owner_principal_id, seq)
);
CREATE INDEX calendar_imports_owner_index ON calendar_imports (owner_principal_id, seq);

CREATE TABLE calendar_events (
    seq                bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text    NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    calendar_seq       bigint  NOT NULL,
    -- uid and recurrence_id are the source's key for the event. recurrence_id
    -- is '' for a single event or a series, and the original start of the one
    -- date a changed copy replaces, as the source wrote it.
    uid                text    NOT NULL,
    recurrence_id      text    NOT NULL DEFAULT '',
    -- source_event_id is Google's own id for the event, set once a Google
    -- import has seen it; '' for an event only an .ics file brought.
    source_event_id    text    NOT NULL DEFAULT '',
    summary            text    NOT NULL DEFAULT '',
    description        text    NOT NULL DEFAULT '',
    location           text    NOT NULL DEFAULT '',
    -- An all-day event has all_day true, its dates in start_date and end_date
    -- (end exclusive, as iCalendar has it), and starts_at/ends_at at UTC
    -- midnight of those dates, for ordering.
    starts_at          bigint  NOT NULL,
    ends_at            bigint  NOT NULL DEFAULT 0,
    all_day            boolean NOT NULL DEFAULT false,
    start_date         text    NOT NULL DEFAULT '',
    end_date           text    NOT NULL DEFAULT '',
    time_zone          text    NOT NULL DEFAULT '',
    recurrence_rule    text    NOT NULL DEFAULT '',
    status             text    NOT NULL DEFAULT '',
    transparency       text    NOT NULL DEFAULT '',
    organizer_email    text    NOT NULL DEFAULT '',
    organizer_name     text    NOT NULL DEFAULT '',
    source_created_at  bigint  NOT NULL DEFAULT 0,
    source_modified_at bigint  NOT NULL DEFAULT 0,
    sequence           bigint  NOT NULL DEFAULT 0,
    -- raw is the whole VEVENT as the source wrote it, so nothing the columns
    -- leave out is lost; raw_hash decides whether an import changes the row.
    raw                text    NOT NULL,
    raw_hash           text    NOT NULL,
    -- The import that last wrote the row.
    import_seq         bigint  NOT NULL,
    created_at         bigint  NOT NULL,
    updated_at         bigint  NOT NULL,
    UNIQUE (calendar_seq, uid, recurrence_id),
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, calendar_seq) REFERENCES calendars (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, import_seq) REFERENCES calendar_imports (owner_principal_id, seq)
);
CREATE UNIQUE INDEX calendar_events_source_event_id ON calendar_events (calendar_seq, source_event_id) WHERE source_event_id <> '';
CREATE INDEX calendar_events_starts_at ON calendar_events (starts_at, seq);
CREATE INDEX calendar_events_owner_starts_at ON calendar_events (owner_principal_id, starts_at, seq);

CREATE TABLE calendar_event_attendees (
    owner_principal_id   text    NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    event_seq            bigint  NOT NULL,
    position             integer NOT NULL,
    email                text    NOT NULL DEFAULT '',
    name                 text    NOT NULL DEFAULT '',
    role                 text    NOT NULL DEFAULT '',
    participation_status text    NOT NULL DEFAULT '',
    PRIMARY KEY (event_seq, position),
    FOREIGN KEY (owner_principal_id, event_seq) REFERENCES calendar_events (owner_principal_id, seq)
);

-- Who an event was with, as people-store ids in the owner's own records.
-- linked_by says how the link was made (GET /vocabularies); attendee-email
-- links are made by every import from the attendees' addresses and remade
-- when the event changes.
CREATE TABLE calendar_event_people (
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    event_seq          bigint NOT NULL,
    person_id          text   NOT NULL,
    linked_by          text   NOT NULL CHECK (linked_by IN ('attendee-email', 'agent', 'operator')),
    reason             text   NOT NULL DEFAULT '',
    linked_at          bigint NOT NULL,
    PRIMARY KEY (event_seq, person_id),
    FOREIGN KEY (owner_principal_id, event_seq) REFERENCES calendar_events (owner_principal_id, seq)
);
CREATE INDEX calendar_event_people_person ON calendar_event_people (owner_principal_id, person_id);

-- Before an import changes an event, the whole event as the API answered it,
-- as JSON text, kept byte for byte.
CREATE TABLE calendar_event_revisions (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    event_seq          bigint NOT NULL,
    reason             text   NOT NULL,
    replaced_at        bigint NOT NULL,
    event_json         text   NOT NULL,
    FOREIGN KEY (owner_principal_id, event_seq) REFERENCES calendar_events (owner_principal_id, seq)
);
CREATE INDEX calendar_event_revisions_event ON calendar_event_revisions (event_seq, seq);
