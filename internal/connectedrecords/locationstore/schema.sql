-- location-store schema. Create-only and idempotent: Open() runs this whole
-- file on every boot. A new column goes in an additive migration in store.go,
-- never in an edit to a CREATE TABLE below.
--
-- Nothing a person can cite is hard-deleted: DELETE stamps deleted_at, and
-- every update, import change, delete and restore of a place, visit or trip
-- first copies the whole row into revisions.

PRAGMA foreign_keys = ON;

-- One imported file, found again by its hash: importing the same file twice
-- changes nothing. finished_at is 0 while an import is still sending; running
-- the importer again on the same file resumes it.
CREATE TABLE IF NOT EXISTS imports (
    id              TEXT PRIMARY KEY,              -- import_000001
    seq             INTEGER NOT NULL UNIQUE,
    kind            TEXT NOT NULL,                 -- vocabulary.go ImportKinds
    sha256          TEXT NOT NULL UNIQUE,          -- of the file (or of the records, for a photo-store import)
    source_machine  TEXT NOT NULL DEFAULT '',      -- where the file was read
    source_path     TEXT NOT NULL DEFAULT '',      -- its path there
    imported_at     INTEGER NOT NULL,
    finished_at     INTEGER NOT NULL DEFAULT 0,
    counts          TEXT NOT NULL DEFAULT '{}'     -- ImportCounts as JSON, summed over every batch
);

-- A named spot.
CREATE TABLE IF NOT EXISTS places (
    id              TEXT PRIMARY KEY,              -- place_000001
    seq             INTEGER NOT NULL UNIQUE,
    name            TEXT NOT NULL DEFAULT '',
    latitude        REAL NOT NULL,
    longitude       REAL NOT NULL,
    radius_meters   REAL NOT NULL DEFAULT 0,       -- 0 = not given
    address         TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL,                 -- vocabulary.go PlaceKinds
    google_place_id TEXT,                          -- NULL when no source gave one; an import finds the place again by it
    description     TEXT NOT NULL DEFAULT '',
    described_by    TEXT NOT NULL DEFAULT '',
    tags            TEXT NOT NULL DEFAULT '[]',
    source          TEXT NOT NULL,                 -- vocabulary.go RecordSources
    import_id       TEXT REFERENCES imports(id),   -- the import that created it; NULL when made by hand
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_places_google ON places(google_place_id) WHERE google_place_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_places_kind ON places(deleted_at, kind);

-- Being somewhere from started_at to ended_at.
CREATE TABLE IF NOT EXISTS visits (
    id                   TEXT PRIMARY KEY,         -- visit_000001
    seq                  INTEGER NOT NULL UNIQUE,
    place_id             TEXT REFERENCES places(id),
    started_at           INTEGER NOT NULL,
    ended_at             INTEGER NOT NULL,
    latitude             REAL,
    longitude            REAL,
    probability          REAL,                     -- 0..1, how sure the source is of the place; NULL when it gave none
    source_semantic_type TEXT NOT NULL DEFAULT '', -- the source's own word for the place (HOME, TYPE_WORK…)
    description          TEXT NOT NULL DEFAULT '',
    described_by         TEXT NOT NULL DEFAULT '',
    tags                 TEXT NOT NULL DEFAULT '[]',
    source               TEXT NOT NULL,
    source_ref           TEXT NOT NULL DEFAULT '', -- what an import finds the visit again by; '' when made by hand
    import_id            TEXT REFERENCES imports(id),
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    deleted_at           INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_visits_source_ref ON visits(source, source_ref) WHERE source_ref != '';
CREATE INDEX IF NOT EXISTS idx_visits_time ON visits(deleted_at, started_at, seq);
CREATE INDEX IF NOT EXISTS idx_visits_place ON visits(place_id, started_at);

-- A movement. parent_trip_id groups the drives of a road trip or a holiday
-- under one trip made by hand; an import never sets it.
CREATE TABLE IF NOT EXISTS trips (
    id               TEXT PRIMARY KEY,             -- trip_000001
    seq              INTEGER NOT NULL UNIQUE,
    parent_trip_id   TEXT REFERENCES trips(id),
    started_at       INTEGER NOT NULL,
    ended_at         INTEGER NOT NULL,
    start_latitude   REAL,
    start_longitude  REAL,
    end_latitude     REAL,
    end_longitude    REAL,
    start_place_id   TEXT REFERENCES places(id),
    end_place_id     TEXT REFERENCES places(id),
    mode             TEXT NOT NULL,                -- vocabulary.go TripModes
    source_mode      TEXT NOT NULL DEFAULT '',     -- the source's own word (IN_PASSENGER_VEHICLE)
    distance_meters  REAL,
    probability      REAL,                         -- 0..1, how sure the source is of the mode
    title            TEXT NOT NULL DEFAULT '',
    description      TEXT NOT NULL DEFAULT '',
    described_by     TEXT NOT NULL DEFAULT '',
    tags             TEXT NOT NULL DEFAULT '[]',
    source           TEXT NOT NULL,
    source_ref       TEXT NOT NULL DEFAULT '',
    import_id        TEXT REFERENCES imports(id),
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    deleted_at       INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_trips_source_ref ON trips(source, source_ref) WHERE source_ref != '';
CREATE INDEX IF NOT EXISTS idx_trips_time ON trips(deleted_at, started_at, seq);
CREATE INDEX IF NOT EXISTS idx_trips_parent ON trips(parent_trip_id, started_at);

-- A trip's path, in order. An import replaces it whole when the trip changes.
CREATE TABLE IF NOT EXISTS trip_path_points (
    trip_id    TEXT NOT NULL REFERENCES trips(id),
    position   INTEGER NOT NULL,
    latitude   REAL NOT NULL,
    longitude  REAL NOT NULL,
    timestamp  INTEGER,                            -- NULL when the source gives the point no time
    PRIMARY KEY (trip_id, position)
) WITHOUT ROWID;

-- Raw location fixes; there may be millions. No public ids: a point is read by
-- time. The unique index both serves time ranges and makes a second copy of a
-- fix from the same source a no-op. A point with a source_ref (a photo id)
-- replaces any earlier point with the same source and source_ref.
CREATE TABLE IF NOT EXISTS points (
    timestamp        INTEGER NOT NULL,
    latitude         REAL NOT NULL,
    longitude        REAL NOT NULL,
    accuracy_meters  REAL,
    source           TEXT NOT NULL,                -- vocabulary.go PointSources
    source_ref       TEXT NOT NULL DEFAULT '',
    import_id        TEXT NOT NULL REFERENCES imports(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_points_identity ON points(timestamp, latitude, longitude, source);
CREATE INDEX IF NOT EXISTS idx_points_source_ref ON points(source, source_ref) WHERE source_ref != '';

CREATE TABLE IF NOT EXISTS revisions (
    revision_id  INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type  TEXT NOT NULL,                    -- place | visit | trip
    entity_id    TEXT NOT NULL,
    reason       TEXT NOT NULL,                    -- update | import | delete | restore | attach
    replaced_at  INTEGER NOT NULL,
    row_json     TEXT NOT NULL                     -- the whole row as it was, as JSON
);
CREATE INDEX IF NOT EXISTS idx_revisions_entity ON revisions(entity_id, revision_id DESC);

-- A stretch of time with one home base: the place the user slept most
-- nights. location-group-journeys writes these from the visits, and a
-- re-run updates the ones it wrote. ended_at 0 runs on past the newest data.
-- A level above journeys, when there is one, is likely to start from these.
CREATE TABLE IF NOT EXISTS home_periods (
    id              TEXT PRIMARY KEY,              -- home_period_000001
    seq             INTEGER NOT NULL UNIQUE,
    place_id        TEXT REFERENCES places(id),    -- NULL when the nights there named no place
    latitude        REAL NOT NULL,
    longitude       REAL NOT NULL,
    locality        TEXT,                          -- Locality as JSON; NULL until named
    started_at      INTEGER NOT NULL,
    ended_at        INTEGER NOT NULL DEFAULT 0,
    nights_known    INTEGER NOT NULL DEFAULT 0,
    nights_at_home  INTEGER NOT NULL DEFAULT 0,
    described_by    TEXT NOT NULL,                 -- who found it: location-group-journeys
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_home_periods_time ON home_periods(deleted_at, started_at);

-- Which visits and trips each import's file holds, whether the import made
-- them, changed them or found them as they were. A full-snapshot export
-- supersedes what an older one held in its span and it does not.
CREATE TABLE IF NOT EXISTS import_records (
    import_id    TEXT NOT NULL REFERENCES imports(id),
    entity_type  TEXT NOT NULL,                    -- visit | trip
    entity_id    TEXT NOT NULL,
    PRIMARY KEY (import_id, entity_type, entity_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_import_records_entity ON import_records(entity_id);

-- A calendar event tied to where the user was for it. The event is Google
-- Calendar's, found again by calendar_id and event_id; summary and location
-- are carried for display only and never read back to find anything.
-- location-match-calendar rewrites only links whose matched_by is its own,
-- and never makes again a link a person deleted (deleted_by 'hand').
CREATE TABLE IF NOT EXISTS calendar_event_links (
    id                  TEXT PRIMARY KEY,          -- calendar_event_link_000001
    seq                 INTEGER NOT NULL UNIQUE,
    calendar_id         TEXT NOT NULL,
    event_id            TEXT NOT NULL,
    recurring_event_id  TEXT NOT NULL DEFAULT '',
    summary             TEXT NOT NULL DEFAULT '',
    location            TEXT NOT NULL DEFAULT '',
    started_at          INTEGER NOT NULL,
    ended_at            INTEGER NOT NULL,
    all_day             INTEGER NOT NULL DEFAULT 0,
    target_type         TEXT NOT NULL,             -- visit | trip | place
    target_id           TEXT NOT NULL,
    rule                TEXT NOT NULL,             -- vocabulary.go CalendarMatchRules
    confidence          REAL NOT NULL,
    reason              TEXT NOT NULL DEFAULT '',
    matched_by          TEXT NOT NULL,             -- location-match-calendar | hand
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    deleted_at          INTEGER NOT NULL DEFAULT 0,
    deleted_by          TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_calendar_event_links_identity ON calendar_event_links(calendar_id, event_id, target_type, target_id);
CREATE INDEX IF NOT EXISTS idx_calendar_event_links_target ON calendar_event_links(target_id, deleted_at);
CREATE INDEX IF NOT EXISTS idx_calendar_event_links_time ON calendar_event_links(deleted_at, started_at);

-- What location-match-calendar found for each recurring series with no
-- location: the spot the user was at for most of its times they were out,
-- how many times agreed, and whether that was enough to link. One row per
-- series, rewritten by each run that reads it. place_id is the leading
-- spot's place even when the outcome is not linked, so a person can see how
-- near it came.
CREATE TABLE IF NOT EXISTS calendar_series_places (
    id                    TEXT PRIMARY KEY,          -- calendar_series_place_000001
    seq                   INTEGER NOT NULL UNIQUE,
    calendar_id           TEXT NOT NULL,
    recurring_event_id    TEXT NOT NULL,
    summary               TEXT NOT NULL DEFAULT '',  -- for display only
    place_id              TEXT REFERENCES places(id),
    spot_place_ids        TEXT NOT NULL DEFAULT '[]',-- JSON array: the places within reach of place_id that count as the same spot
    instances             INTEGER NOT NULL,          -- timed times of the series in the window read
    instances_out         INTEGER NOT NULL,          -- of those, times the user was at a place out of home, not away
    instances_at_place    INTEGER NOT NULL,          -- of those, times at the spot
    agreement             REAL NOT NULL,             -- instances_at_place / instances_out, 0 when none
    confidence            REAL NOT NULL,             -- what the links carry; 0 unless linked
    outcome               TEXT NOT NULL,             -- vocabulary.go SeriesOutcomes
    first_instance_at     INTEGER NOT NULL,
    last_instance_at      INTEGER NOT NULL,
    window_from           INTEGER NOT NULL,          -- the run's window
    window_to             INTEGER NOT NULL,
    matched_by            TEXT NOT NULL,             -- location-match-calendar
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_calendar_series_places_series ON calendar_series_places(calendar_id, recurring_event_id);
CREATE INDEX IF NOT EXISTS idx_calendar_series_places_place ON calendar_series_places(place_id);

-- A name a place might have, found offline by location-suggest-place-names
-- (an OpenStreetMap feature or address near it, or calendar events at an
-- address there). Never applied by a run: accepting one through the routes
-- names the place. A run rewrites only proposed ones, and deletes proposed
-- ones it no longer finds; accepted and rejected ones stay as decided.
CREATE TABLE IF NOT EXISTS place_suggestions (
    id              TEXT PRIMARY KEY,              -- place_suggestion_000001
    seq             INTEGER NOT NULL UNIQUE,
    place_id        TEXT NOT NULL REFERENCES places(id),
    source          TEXT NOT NULL,                 -- vocabulary.go PlaceSuggestionSources
    source_ref      TEXT NOT NULL,                 -- node/123, way/456, calendar:node/789
    name            TEXT NOT NULL DEFAULT '',
    address         TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL DEFAULT '',      -- '' leaves the place's kind alone
    distance_meters REAL NOT NULL DEFAULT 0,
    reason          TEXT NOT NULL DEFAULT '',
    evidence        TEXT NOT NULL DEFAULT '{}',    -- PlaceSuggestionEvidence as JSON
    status          TEXT NOT NULL,                 -- proposed | accepted | rejected
    proposed_by     TEXT NOT NULL,
    decided_by      TEXT NOT NULL DEFAULT '',
    decided_at      INTEGER NOT NULL DEFAULT 0,
    decision_note   TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_place_suggestions_identity ON place_suggestions(place_id, source, source_ref);
CREATE INDEX IF NOT EXISTS idx_place_suggestions_status ON place_suggestions(status, deleted_at);
