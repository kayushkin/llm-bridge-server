# location-store — API contract

`127.0.0.1:8333`, loopback only, no auth: a front door such as dash adds it.
Where the user has been: named places, visits to them, trips between them, the
raw location points underneath, and the imports that brought them in. SQLite
at `<data dir>/location-store.db` (default `~/.config/location-store`,
directory 0700, file 0600). Timestamps are epoch seconds.

## Settings

| Variable | Default | What it is |
|---|---|---|
| `LOCATION_STORE_ADDR` | `127.0.0.1:8333` | listen address |
| `LOCATION_STORE_DATA_DIR` | `~/.config/location-store` | holds `location-store.db` |
| `LOCATION_STORE_URL` | `http://127.0.0.1:8333` | the commands: where location-store answers |
| `PHOTO_STORE_URL` | `http://127.0.0.1:8329` | `location-import-photos`: where photo-store answers |
| `DASH_URL` | `http://localhost:8101` | `location-match-calendar`: where dash answers, the one way to Google Calendar |
| `DASH_TOKEN` | (secret, required) | `location-match-calendar` and `location-suggest-place-names`: dash's bearer token, sent only to `DASH_URL`, never printed. The service itself never holds it |
| `LOCATION_STORE_OSM_EXTRACTS` | `~/.cache/osm` | `location-suggest-place-names`: OpenStreetMap extracts (`.osm.pbf`), comma-separated files or directories of them. Never set it in the service's own environment |

## Records

Places, visits and trips have ids from this store (`place_000001`,
`visit_000001`, `trip_000001`), registered in kanban-store's entity-type
registry as types `place`, `visit` and `trip`, so a timeline item or a note
cites one as a chip. Each update, import change, delete and restore first
copies the whole record to `revisions`; deletes are soft.

**An import never touches** a name, title, description, tags,
`parent_trip_id` or locality, never changes a place link already set, never
changes a trip's `mode` once it no longer matches what the source said
(someone corrected it), and leaves a record someone deleted deleted. A
record a newer export superseded is brought up to date, and back, when an
export holds it again (see *Snapshots* under Importing).

**Who wrote it.** A generated value names who generated it, so a later run
rewrites only its own: a locality's `described_by`, a trip's `title_by` and
`parent_set_by`, a home period's `described_by`. Through the routes these
are `hand`; the commands write their own names (`location-name-localities`,
`location-group-journeys`).

### Localities

A `locality` (on places and visits; `start_locality` and `end_locality` on
trips) is the town a coordinate is in: `{city, region, country,
country_code, time_zone, geonames_id, distance_meters, described_by}`, null
until named. `location-name-localities` names them offline from GeoNames'
`cities1000` (towns of 1000 people or more, parts of towns left out) with
first-level region and country names, downloaded once to `<data
dir>/geonames`: of the towns whose reach (15 m × √population) covers the
coordinate, the most populous, else the nearest. A visit is named from its
own coordinate, else its place's. Filling never replaces a locality already
set, and copies the record to `revisions` first (reason `locality`).

### `places`

| Field | Meaning |
|---|---|
| `id` | `place_000001` |
| `name`, `address` | an import fills them only while blank |
| `latitude`, `longitude`, `radius_meters` | `radius_meters` 0 = not given |
| `kind` | `GET /vocabularies` `place_kinds`: `home`, `work`, `venue`, `saved`, `other`. An import turns `other` into what the source says, never anything else; `location-group-journeys` sets a home period's place to `home` unless someone set its kind by hand |
| `google_place_id` | Google's id, when a source gave one; unique; an import finds the place again by it |
| `description`, `described_by` | markdown; `described_by` (who wrote it) is required with a description |
| `tags` | JSON array of strings |
| `source`, `import_id` | `google_timeline`, `google_semantic`, `manual` or `generated`; the import that created it |
| `locality` | the town; see *Localities* |
| `deleted_at` | 0 = live |
| `merged_into_place_id` | the place this one was merged into, null if never merged; an import that names this place by `google_place_id` lands on that one. A restore clears it |

### `visits`

| Field | Meaning |
|---|---|
| `id` | `visit_000001` |
| `place_id`, `place_name` | the place, nullable; `place_name` is read with the visit, for display |
| `started_at`, `ended_at` | |
| `latitude`, `longitude` | where the source put the visit, nullable |
| `probability` | 0..1, how sure the source is of the place, nullable |
| `source_semantic_type` | the source's own word (`HOME`, `TYPE_WORK`, `UNKNOWN`) |
| `description`, `described_by`, `tags` | as on places |
| `source`, `source_ref`, `import_id` | an import finds the visit again by `source` and `source_ref` (`<started_at>-<ended_at>`) |
| `superseded_by_import_id` | the newer export that no longer holds the visit, when that is why it is deleted |
| `locality` | the town; see *Localities* |

### `trips`

| Field | Meaning |
|---|---|
| `id` | `trip_000001` |
| `kind` | `GET /vocabularies` `trip_kinds`: `movement` (one drive, flight or walk a source recorded) or `journey` (a span away from home made of movements: by hand, or by `location-group-journeys` with `source` `generated`) |
| `parent_trip_id`, `parent_set_by` | the journey this trip is part of, never set by an import; who set or cleared it: `""` nobody yet, `hand`, or `location-group-journeys` |
| `started_at`, `ended_at` | |
| `start_latitude` … `end_longitude` | nullable, in pairs |
| `start_place_id`, `end_place_id` | nullable; `start_place_name`, `end_place_name` for display |
| `mode` | `GET /vocabularies` `trip_modes`: `driving`, `motorcycling`, `walking`, `running`, `hiking`, `cycling`, `bus`, `train`, `tram`, `subway`, `ferry`, `boating`, `flying`, `skiing`, `mixed`, `unknown` |
| `source_mode` | the source's own word (`IN_PASSENGER_VEHICLE`) |
| `distance_meters`, `probability` | nullable; `probability` is 0..1, how sure the source is of the mode |
| `title`, `title_by` | `title_by` is who wrote the title: `hand`, or the command that generated it |
| `description`, `described_by`, `tags` | |
| `source`, `source_ref`, `import_id`, `superseded_by_import_id` | as on visits |
| `start_locality`, `end_locality` | the towns; a journey's are its first and last movement's |
| `path_points` | how many points `GET /trips/{id}/path` answers |
| `child_trips` | how many live trips name this one as their parent |

### `home_periods`

A stretch of time with one home base: where the user slept most nights.
`location-group-journeys` writes them; a re-run updates its own (same site,
overlapping time) and soft-deletes the ones it no longer finds. A level
above journeys, when there is one, is likely to start from these.

| Field | Meaning |
|---|---|
| `id` | `home_period_000001` |
| `place_id`, `place_name` | the home; null when its nights named no place |
| `latitude`, `longitude`, `locality` | where it is |
| `started_at`, `ended_at` | `ended_at` 0: runs on past the newest data. Each starts where the one before ends |
| `nights_known`, `nights_at_home` | nights the data places anywhere, and of those at the home |
| `described_by` | `location-group-journeys` |

### Journeys

`location-group-journeys [-dry-run] [-away-km 80] [-minimum-hours 6]
[-home-change-days 21] [-return-break-hours 12]` (or `POST /journeys/group`):

1. **Nights.** Each night (03:00 in the time zone of the town nearest the
   visit) is placed at the visit that covers it; places within 300 m are one
   site.
2. **Home periods.** The home of a day is the site of most of the known
   nights in the 21 nights around it, when it holds at least half. A lead
   shorter than 21 days is dropped (a holiday, a visit to family), so a home
   changes only when a new site leads for three weeks or more.
3. **Journeys.** A visit or movement is *away* when it is more than 80 km
   from the home in force, or is a flight. Away entries join into one
   journey unless the record shows the user back within 80 km of home for 12
   hours or overnight; a stretch the record is silent through does not split
   one. Each journey's edges take in the short stops (an hour or less, not at
   home) and movements that lead out of and back into it. A span is a
   journey when it holds a night, a flight, or lasts 6 hours. Its trip has
   kind `journey`, source `generated`, mode the one covering 80% of its
   distance (else `mixed`), and title `Home town → Town, Region → …, 26 Jun
   – 30 Jun 2026`: the towns it slept in (at most three, by time spent), else
   the towns of stops of 3 hours or more, else its farthest town; abroad the
   country stands in for the region. Its children are the movements in the
   span.
4. **Re-running** matches each journey found to the stored generated journey
   it overlaps most: it updates times, ends, mode and distance, rewrites the
   title only while `title_by` is `location-group-journeys`, attaches
   movements no one has placed, moves movements only it placed, and takes out
   of a journey the movements it placed there that no longer belong. A
   journey it made that nothing matches is soft-deleted, unless someone has
   written on it or placed a trip in it by hand. A journey made by hand that
   overlaps one found makes it skip that one, and a movement whose parent was
   set by hand stays where it is. Each change is copied to `revisions`
   (reason `journey`).

### `calendar_event_links`

A Google Calendar event tied to where the user was for it. The event is
Google's, found again by `calendar_id` and `event_id` and never by its
summary; one link per event and record.

| Field | Meaning |
|---|---|
| `id` | `calendar_event_link_000001` |
| `calendar_id`, `event_id` | Google's ids: what the link is found again by |
| `recurring_event_id` | the series the event is an instance of; `""` for a single event |
| `summary`, `location` | the event's as last read, for display only |
| `started_at`, `ended_at`, `all_day` | the event's times; an all-day event's are its local days |
| `target_type`, `target_id` | `visit`, `trip` or `place`, and its id |
| `rule`, `confidence`, `reason` | `GET /vocabularies` `calendar_match_rules`; 0..1; in words, what agreed |
| `matched_by` | `location-match-calendar`, or `hand` (through the routes) |
| `deleted_at`, `deleted_by` | soft delete; a link `deleted_by` `hand` is never made again |

### Calendar events

`location-match-calendar [-from YYYY-MM-DD] [-to YYYY-MM-DD] [-calendar
<id>,…] [-dry-run] [-all]` (default the last 90 days) reads every calendar
dash lists, and their events in the window, and sends them to `POST
/calendar/match`, which decides. Run it last: import, then
`location-name-localities`, then `location-group-journeys`, then this. Nothing
is geocoded by an outside service: an event's location is matched against
the names and addresses of the store's places, and against GeoNames town
names on disk. Each event is decided by the first rule that applies:

1. **Not a plan.** Canceled events, and Google's `birthday`,
   `workingLocation`, `outOfOffice` and `focusTime` events, link nothing.
2. **Flights.** A summary `Flight to X (AA 123)` links to the `flying`
   movement overlapping it most (`flight`, 0.9) and to that movement's
   journey (`flight_journey`, 0.8); with no flight recorded, to the journey
   it falls in (`flight_journey`, 0.6).
3. **Video calls.** A location naming Teams, Zoom, Google Meet, Webex or
   Skype, or no location and a Meet link, links nothing.
4. **A known place.** When a part of the location (split at commas,
   semicolons and brackets) is a place's whole name, or the location holds
   the street line of a place's address (and its town), the visit near that
   place (its place, or within its radius, at least 150 m) that overlaps the
   event most links (`place`, 0.9). Overlap means half the event, or half an
   hour of a longer one. Recurring or not.
5. **A town.** Otherwise the first part after the first that is a GeoNames
   town's name (narrowed by a region after it: `NV`, `Nevada`) names the
   town; a visit is in it when its locality has that name or it lies within
   the town's reach (at least 5 km) plus 3 km. Visits at home (a `home`
   place, or within 300 m of the home period in force) never count.
   - A **recurring** event links only when its series has one place the
     user was at for at least 3 of its times, and at least half the times
     they were anywhere in that town but home (`series_place`, 0.7). A
     routine never links by time alone.
   - A **single timed** event links to a visit in the town that began within
     an hour of its start (`town_visit`, 0.5).
   - Else a single event links to a journey that was in the town, more than
     80 km from home, while it ran (`town_journey`, 0.5).
6. **No location.** A single event with no location (or only a web
   address), on a calendar whose `accessRole` is `owner` or `writer`, links
   to the journey it falls in (`journey_time`, 0.4); a calendar the user only
   reads holds other people's days off. A recurring one never links by time
   alone, only by the spot its series is held (next).

### Recurring series with no location

A weekly class with no location says nothing by itself, but the times the
user went agree on one spot. For each recurring series (calendar id and
`recurringEventId`; never the summary) of timed events with no location:

- A time finds the user **out** when a visit not at home (a `home` place,
  or within 300 m of the home period in force) and not away (more than
  80 km from home) began no later than 45 minutes after its start and spans
  enough of it (half, or half an hour). A visit that began hours before (a
  night somewhere, a day at work) shows the user was there instead.
- The user **arrived for** a time at a visit that began within 45 minutes of
  its start, either way. A **spot** is a place and every place within 150 m
  of it: one building often has two Google place ids. Spots holding a
  `home` or `work` place are never one.
- The spot arrived at for the most times leads (ties: the place arrived at
  itself most often). The series is held there when that is at least 3 times
  and at least 60% of the times out. Each time the user arrived there links
  to that visit (`unlocated_series_place`, confidence 0.4 + 0.4 × the share:
  0.64 to 0.8); the other times link nothing.

Every series read is recorded, linked or not, in `calendar_series_places`
(`calendar_series_place_000001`), one row per series rewritten by each run
that reads it: `place_id` (the leading spot's place, null when none),
`place_name`, `spot_place_ids`, `instances` (times in the window),
`instances_out`, `instances_at_place`, `agreement`, `confidence` (0 unless
linked), `outcome` (`series_outcomes`: `linked`, `below_agreement`,
`too_few_times`, `never_out`), `first_instance_at`, `last_instance_at`, and
the run's `window_from` and `window_to`. A dry run writes none; `POST
/calendar/match` answers them as `series_places`.

A run settles only its own links of the calendars it was sent, for events in
its window: it makes new ones, rewrites changed ones, deletes ones it no
longer finds (an event moved or removed), and restores ones it deleted
itself. A link made by hand is left as it is, and one a person deleted stays
deleted. No run writes a name, title or description.

### Place name suggestions

`location-suggest-place-names [-extract <file or directory>]… [-limit 30]
[-from YYYY-MM-DD] [-to YYYY-MM-DD] [-dry-run]` suggests names for the
places the user goes to that have none, offline:

1. `GET /unnamed-places?limit=` ranks the live places with no name the user
   visited, most days first, home (kind `home`, or a home period's place)
   left out, each with its visit pattern: visits, days, hours, median visit
   length, first and last visit, and arrivals by weekday and local hour (the
   place's town's zone).
2. It reads the OpenStreetMap extracts (`LOCATION_STORE_OSM_EXTRACTS`, or
   `-extract`; Geofabrik's regional downloads, several at once): named
   features with a type tag (`amenity`, `shop`, `leisure`, `tourism`,
   `office`, `craft`, `healthcare`, `sport`, `club`, `historic`, `building`)
   and every `addr:housenumber` + `addr:street`, ways placed at the mean of
   their nodes. Relations are not read.
3. It reads every calendar through dash and finds each event whose location
   holds a street address in the extracts (`12 N Example St` matches `12
   North Example Street`; a town named after it narrows two towns' matches,
   and matches more than 150 m apart give none).
4. For each place it sends the 10 nearest features within 150 m, the
   nearest address within 150 m and the events whose address lies within
   150 m to `POST /place-suggestions/proposals`, which decides.

No coordinate or address ever goes to an online service. The store makes a
`place_suggestions` row (`place_suggestion_000001`) for each feature within
100 m (the 5 nearest; `osm_poi`, kind `venue`), each calendar address
(`calendar_address`, named by the most common text before the street
address, and saying how many of its events a visit at the place overlapped)
and the nearest address within 60 m (`osm_address`, named by the address,
kind left alone). Each carries `reason` and `evidence`: the place's visit
pattern, everything found near it, and `pois_within_25_meters`, so a
decider sees the rivals. A run rewrites only `proposed` suggestions, keyed by
place, source and `source_ref` (`node/123`, `way/456`,
`calendar:node/789`); deletes proposed ones it no longer finds, and those of
a place someone has since named; and never touches one `accepted` or
`rejected`. Proposing for a named or deleted place is a 409.

**Nothing is named by a run.** `POST /place-suggestions/{id}/accept
{decided_by, note, name?, address?, kind?}` names the place (the optional
fields replace the suggestion's own), sets its address only when it has
none and its kind only when it is `other`, writes a description naming the
suggestion and its reason (with `described_by` the decider) when it has
none, copies the place to `revisions` first (reason `suggestion`), and
rejects the place's other proposed suggestions, naming this one. A place
already named is a 409: a suggestion never renames. `POST
/place-suggestions/{id}/reject {decided_by, note}` records a wrong one.

### `points`

Raw fixes, possibly millions; no public ids. `timestamp`, `latitude`,
`longitude`, `accuracy_meters` (nullable), `source` (`point_sources`:
`google_records`, `google_timeline_raw`, `google_timeline_path`,
`google_semantic_path`, `photo`, `google_activity`, `google_photos_sidecar`,
`phone`),
`source_ref` (a photo id for `photo` and `google_photos_sidecar`, an
activity-store activity id for `google_activity`, else `""`), `import_id`. A second copy of a fix from the same source is a no-op; a
point with a `source_ref` replaces an earlier one with the same source and
`source_ref`.

### `imports`

`id` (`import_000001`), `kind` (`import_kinds`: `google_timeline`,
`google_records`, `google_semantic`, `photo_store`, `activity_store`,
`google_photos_sidecars`, `phone_live`), `sha256` (of the file;
unique), `source_machine`, `source_path`, `recorded_by` (who sent a live
import's points, as the front door signed it; `""` for a file), `imported_at`, `finished_at` (0
while sending), `counts` (`points_added`, `points_unchanged`,
`places_created`, `places_updated`, `visits_created`, `visits_updated`,
`trips_created`, `trips_updated`, `records_unchanged`).

## Routes

Lists take `?limit=` (default 100, max 1000) and `?offset=`. A time range
`?from=&to=` selects what overlaps it: ends at or after `from`, starts before
`to`; either may be left out. `?include_deleted=true` shows deleted records.
A `PATCH` with an unknown key, or `null` on a field that is not a link, is a
400.

| Method | Path | |
|---|---|---|
| `GET` | `/health` | `{status, counts:{places, visits, trips, points, imports, deleted, revisions}}` |
| `GET` | `/settings` | the declared settings |
| `GET` | `/vocabularies` | `{place_kinds, trip_kinds, trip_modes, record_sources, point_sources, import_kinds, calendar_match_rules, calendar_link_target_types, series_outcomes, place_suggestion_statuses, place_suggestion_sources}` |
| `GET` | `/places` | oldest first. `?q=` (every word in name, address, description or tags), `?kind=`, `?tag=`, `?google_place_id=` |
| `POST` | `/places` | `{name, latitude, longitude, radius_meters, address, kind, description, described_by, tags}`; name, latitude, longitude, kind required. **201** |
| `GET` | `/places/{id}` | deleted is 404 unless `?include_deleted=true` |
| `PATCH` | `/places/{id}` | any of `name`, `latitude`, `longitude`, `radius_meters`, `address`, `kind`, `description`, `described_by`, `tags` |
| `POST` | `/places/{id}/merge` | `{into}`: two records of one real place. Every visit, trip start and end, calendar link to the place (a link whose event already links to `into` is soft-deleted, `deleted_by` `merge`), recurring-series spot and home period that named `{id}` names `into`; a place merged into `{id}` earlier points at `into`; then `{id}` is soft-deleted with `merged_into_place_id`. Each moved record is first copied to `revisions` with reason `merge`, which is not a hand edit, so imports and phone segmenting still treat it as theirs. `into`'s own fields are untouched. Answers `{place, merged_place_id, visits_moved, trip_starts_moved, trip_ends_moved, calendar_links_moved, calendar_links_duplicate, calendar_series_moved, home_periods_moved, places_repointed}`. **409** if either is deleted |
| `GET` | `/visits` | earliest first. `?from=&to=`, `?place_id=`, `?source=`, `?tag=` |
| `POST` | `/visits` | `{place_id, started_at, ended_at, latitude, longitude, description, described_by, tags}`; a place or a coordinate required. **201** |
| `GET` | `/visits/{id}` | with `calendar_event_links`, the live links to it (lists leave them out) |
| `PATCH` | `/visits/{id}` | any of `place_id` (null clears), `description`, `described_by`, `tags` |
| `GET` | `/trips` | earliest first. `?from=&to=`, `?mode=`, `?parent_trip_id=` (an id lists its children; `none` lists trips that are part of no other), `?min_distance_meters=`, `?source=`, `?tag=`, `?exclude_parents=true`, `?kind=` (`movement`, `journey`) |
| `POST` | `/trips` | `{kind, title, started_at, ended_at, mode, parent_trip_id, start_/end_ latitude/longitude, start_place_id, end_place_id, distance_meters, description, described_by, tags}`; title, times and mode required; `kind` is `journey` when left out. Usually a road trip or holiday to attach imported drives to. `title_by` and, with a parent, `parent_set_by` are `hand`. **201** |
| `GET` | `/trips/{id}` | with `calendar_event_links`, the live links to it (lists leave them out) |
| `PATCH` | `/trips/{id}` | any of `parent_trip_id`, `start_place_id`, `end_place_id` (null clears each), `mode`, `title`, `description`, `described_by`, `tags`. A parent that would make a loop is a 400. Patching `title` sets `title_by` to `hand`; patching `parent_trip_id`, to a trip or to null, sets `parent_set_by` to `hand`, so `location-group-journeys` leaves the trip where it was put |
| `GET` | `/trips/{id}/path` | `[{latitude, longitude, timestamp}]` in order; `timestamp` null when the source gave none |
| `POST` | `/trips/{id}/children` | `{"trip_ids":[…]}`: makes each part of this trip, all or none, with `parent_set_by` `hand`. Answers the parent |
| `DELETE` | `/places/{id}`, `/visits/{id}`, `/trips/{id}` | soft, **204**; again is a no-op |
| `POST` | `/places/{id}/restore`, `/visits/{id}/restore`, `/trips/{id}/restore` | answers the record |
| `GET` | `/places/{id}/revisions`, `/visits/{id}/revisions`, `/trips/{id}/revisions` | newest first: `{revision_id, entity_type, entity_id, reason, replaced_at, row}`, `row` the whole record as it was |
| `POST` | `/localities/fill` | `{dry_run}`: names every empty locality (see *Localities*). Answers `{places, visits, trip_starts, trip_ends, already_named, no_coordinate, no_town_nearby, distinct_towns, geonames_towns_in}`. **409** until the GeoNames files are in `<data dir>/geonames` |
| `GET` | `/home-periods` | earliest first; `?include_deleted=true` |
| `GET` | `/home-periods/{id}` | |
| `POST` | `/journeys/group` | `{dry_run, away_distance_meters, minimum_away_seconds, home_change_days, return_break_seconds}`, 0 = default. Answers `{dry_run, request, home_periods, journeys:[{action, trip_id, title, started_at, ended_at, mode, nights, farthest_meters, child_trip_ids, children_attached, children_detached, children_kept_elsewhere, notes}], deleted_journey_ids, home_places_marked, notes}`; `action` is `create`, `update`, `unchanged` or `skip`. A dry run writes nothing and answers `trip_id` `""` for a journey it would create. **409** without the GeoNames files |
| `GET` | `/journeys` | trips of kind `journey`, earliest first; takes `GET /trips`' time range and paging |
| `GET` | `/journeys/{id}` | `{trip, children, visits, localities:[{locality, seconds, arrived_at}], calendar_event_links}`: the trip, the trips that make it up, the visits inside its span, the towns those visits were in, in order of arrival, and the live calendar event links to any of them |
| `POST` | `/calendar/match` | `{dry_run, from, to, calendars:[{calendar:{id, summary, summaryOverride, accessRole, timeZone}, events:[{id, status, eventType, summary, location, recurringEventId, hangoutLink, start:{dateTime, date, timeZone}, end}]}]}`, Google's own field names. Answers `{dry_run, from, to, events_read, links_by_rule, unlinked, actions, links:[{action, link}], series_places, notes}`; `action` is `create`, `update`, `unchanged`, `restore`, `delete` or `kept_deleted`. A dry run writes nothing and answers `id` `""` for a link it would make. **409** without the GeoNames files |
| `GET` | `/calendar-event-links` | earliest event first. `?from=&to=`, `?visit_id=`, `?trip_id=`, `?place_id=` (links to the place and to its visits), `?calendar_id=`, `?event_id=`, `?rule=`, `?matched_by=`, `?include_deleted=true` |
| `POST` | `/calendar-event-links` | `{calendar_id, event_id, recurring_event_id, summary, location, started_at, ended_at, all_day, target_type, target_id, reason}`: a link by hand (`rule` and `matched_by` `hand`, confidence 1); one already there for the same event and record becomes this. **201** |
| `GET` | `/calendar-event-links/{id}` | |
| `DELETE` | `/calendar-event-links/{id}` | soft, **204**, `deleted_by` `hand`: no run makes it again |
| `GET` | `/calendar-series-places` | most times at the spot first. `?place_id=` (series whose spot holds it), `?calendar_id=`, `?outcome=` |
| `GET` | `/calendar-series-places/{id}` | |
| `GET` | `/unnamed-places` | `[{place, pattern}]`, most days visited first, home left out; `?limit=&offset=` |
| `POST` | `/place-suggestions/proposals` | `{dry_run, proposed_by, sources, places:[{place_id, nearby_pois, nearest_address, calendar_events}]}` (`PlaceSuggestionProposals` in `location.go`). Answers `{dry_run, places:[{place, suggestions}], actions}`; `actions` counts `create`, `update`, `unchanged`, `kept_decided`, `delete`. **409** for a named or deleted place |
| `GET` | `/place-suggestions` | oldest first. `?place_id=`, `?status=`, `?source=`, `?include_deleted=true` |
| `GET` | `/place-suggestions/{id}` | |
| `POST` | `/place-suggestions/{id}/accept` | `{decided_by, note, name, address, kind}`, `decided_by` required: names the place (see *Place name suggestions*). **409** unless proposed and the place unnamed |
| `POST` | `/place-suggestions/{id}/reject` | `{decided_by, note}`. **409** unless proposed |
| `GET` | `/timeline` | visits and trips overlapping `?from=&to=`, in start order: `[{kind:"visit"\|"trip", visit, trip}]`. Trips that have children are left out unless `?include_parent_trips=true`. `?source=`; `?limit=` default 500, max 5000 |
| `GET` | `/points` | earliest first. `?from=` (inclusive) `&to=` (exclusive) on `timestamp`, `?source=`, `?source_ref=`; `?limit=` default 1000, max 10000 |
| `POST` | `/points/live` | `{device, recorded_by, points:[{timestamp, latitude, longitude, accuracy_meters}]}`, at most 5000 points: fixes a phone sent as it made them (see *Phone points*). Answers `{import_ids, points_added, points_unchanged}` |
| `POST` | `/phone-points/segment` | `{dry_run, from, to}`, at most 400 days: makes the phone points into visits and movements (see *Phone points*). Answers `{dry_run, from, to, points_read, points_too_rough, places_created, actions:[{action, entity_type, id, started_at, ended_at, place_id, mode, distance_meters, points, note}], action_counts}`; `action` is `create`, `update`, `unchanged`, `delete`, `kept_edited`, `kept_deleted` or `skip_other_source`. A dry run does the work and writes none of it, so the ids it answers are the ones a real run would have made |
| `GET` | `/imports` | newest first |
| `POST` | `/imports` | `{kind, sha256, source_machine, source_path, reapply}`: starts an import, or answers the one with that hash. `{import, already_finished}`. `reapply: true` opens a finished import again (its counts and its record of what the file holds start afresh), so the same file can be sent once more |
| `GET` | `/imports/{id}` | |
| `POST` | `/imports/{id}/batches` | `{places, visits, trips, points}` (`ImportBatch` in `location.go`), one transaction: one bad record is a 400 naming it and writes nothing. Places are written first, so a visit may name a place of the same batch by `google_place_id`. A finished import is a **409**. Answers what it did |
| `POST` | `/imports/{id}/finish` | body empty, or `{snapshot:{covers_from, covers_to, raw_signals_oldest_at, raw_signals_newest_at}}` for an on-device Timeline export, which makes it a full snapshot and reconciles the snapshots (below). Stamps `finished_at`; the same file is then a no-op. Answers the import, with `snapshot` and `snapshot_changes` `{visits_superseded, trips_superseded, visits_restored, trips_restored, kept_edited_ids}` |

## Importing

`~/bin/location-import-google -file <path> [-source-machine <name>] [-dry-run] [-reapply]`
reads a Google export and posts it in batches of 2000. `<path>` is a file or a
directory; a directory imports every `.json` file under it, each as its own
import, and skips files that are not location history. It tells the formats
apart by their keys:

- **On-device Timeline export** (`Timeline.json`, 2024 on). `semanticSegments`:
  a `visit` at `hierarchyLevel` 0 becomes a visit, and its `topCandidate`
  (`placeId`, `semanticType`, `placeLocation`) a place; an `activity` becomes
  a trip (`topCandidate.type` → `mode`, `distanceMeters`), whose path is the
  `timelinePath` points inside its time; every `timelinePath` point is also a
  point. `rawSignals` `position`s become points. `userLocationProfile.frequentPlaces`
  become places (`label` → kind). Visits inside other visits
  (`hierarchyLevel` above 0) and `timelineMemory` are left out and counted.
- **Legacy Takeout `Records.json`**: `locations[]` become points
  (`latitudeE7`/`longitudeE7`, `timestamp` or `timestampMs`, `accuracy`).
- **Legacy Takeout `Semantic Location History/YYYY/YYYY_MONTH.json`**:
  `placeVisit` becomes a visit at its center and, with a `placeId`, a place
  (`name`, `address`, `semanticType`); `activitySegment` becomes a trip whose
  path is `waypointPath`, or `simplifiedRawPath` when there are no waypoints.
  Every `simplifiedRawPath` point is also a point.

### Phone points

A phone sends its fixes through a front door (dash) to `POST /points/live`:
points with source `phone`, held in one import of kind `phone_live` per
`device` and UTC day, found again by a hash of the two. `device` is 1 to 100
letters, digits, dots, dashes or underscores; `recorded_by` is required and
the front door writes it. A fix is refused when its time is before 2000 or
more than 10 minutes past the server's clock. Sending a fix again is a no-op,
so a phone may resend a batch whose answer it lost.

`~/bin/location-segment-phone-points [-days 3] [-dry-run] [-all]` (or
`POST /phone-points/segment`) makes those points into records, source
`phone_points`, all decided in `phone_points.go`:

1. Fixes less accurate than 250 m are left out and counted.
2. A **stay** is a run of fixes within 150 m of the run's center, with no
   silence longer than 12 hours (a phone lying still overnight reports
   seldom, and a night must stay one visit), lasting 10 minutes or more. Two
   stays in a row within 150 m of each other are one.
3. Each stay is a **visit** at the place nearest its center within the
   place's radius (at least 100 m; the oldest of equally near ones); with
   none, a new unnamed place there, kind `other`, source `phone_points`, for
   the naming suggestions to find. A visit already stored **keeps its
   place** while the place is live and the center within its reach, so a
   merge (`POST /places/{id}/merge`) or a near duplicate never moves it. A
   movement starts and ends at its two stays' places.
4. Between two stays is a **movement** (a trip of kind `movement`) whose path
   is the fixes between them. Its mode comes from speed alone: a flight is
   100 km or more as the crow flies at 200 km/h or faster; 25 km/h or faster
   along the path is driving; 7 km/h or slower walking; anything else
   `unknown`.
5. **Re-running** matches each segment to the stored record of the same
   kind it overlaps most, and updates its times, ends, place, mode and path
   (clearing a locality whose coordinate moved). A stored record no segment
   matches is soft-deleted. A record someone **edited** (a description,
   tags, or a revision with reason `update`, `attach` or `restore`) or
   **deleted** is left as it is, and no segment is made over it. No segment
   is made where a visit or movement from any other source already is.
   Every change is copied to `revisions` (reason `phone_points`).
6. A run starts reading at the start of a stored phone visit or movement
   that runs across `from`, so it never cuts a stay in two.

Then run `location-name-localities` → `location-group-journeys` →
`location-match-calendar`, which read these records like an export's.

### Snapshots

An on-device Timeline export holds every visit and activity of the span it
covers, so it is a full snapshot of that span. Two exports of the same span
can cut a day into different segments, and an import finds a record again
only by its exact start and end, so a second export would otherwise add
near-copies beside the first's. The store keeps which visits and trips each
import's file holds (`import_records`), and when an import finishes with a
snapshot it reconciles every `google_timeline` visit and trip:

- Of the snapshot imports whose span (`covers_from` to `covers_to`) holds
  the record's start, **the newest decides**. If it holds the record, the
  record stands, and comes back if a reconcile had set it aside. If not, the
  record is soft-deleted with `superseded_by_import_id` naming that import,
  and its revision's reason is `superseded by import_000003`.
- A record **someone edited** (a revision with reason `update`, `attach` or
  `restore`, or a trip whose `title_by` or `parent_set_by` is `hand`) is
  kept and listed in `kept_edited_ids` instead.
- A record whose start **no snapshot covers** is left alone: a narrower
  export never touches what lies outside its own span.
- **Newest** is decided by the files' own content: the later newest raw
  position, then the later oldest raw position (the phone keeps raw
  positions in a rolling buffer, so of two exports with the same newest
  position the later has dropped more of the oldest), then the later import.
  The end of the last segment does not decide it: the ongoing visit's end is
  not the time of the export.
- A record's fields are those of the last import that sent it, so send
  older exports first. To give imports made before the store kept what each
  file holds a record of it, send each file again, oldest first, with
  `location-import-google -reapply -file <path>`.

Points are not reconciled: a raw fix is a fact whichever export carried it.

The iPhone export (a top-level JSON array) is refused. Records with no
readable time or place are left out and counted in the command's output.

`~/bin/location-import-photos [-date-sources exif,filename,google_photos] [-dry-run]` reads
photo-store's photos with GPS (`GET /photos?has_gps=true`, paged) and makes
each into a point, source `photo`, `source_ref` the photo id. Only photos
dated by a trusted source are read: a photo dated when it was sent or posted
was taken somewhere earlier. The import's hash is that of the points it
would write, so running it again with nothing new changes nothing.

`~/bin/location-import-activity [-bases "From your device"] [-dry-run]` reads
every activity-store activity (`GET /activities`, paged; there is no filter
for coordinates) and makes each with `latitude` and `longitude` into a point,
source `google_activity`, `source_ref` the activity id, at `occurred_at`, with
no accuracy (activity-store keeps none). Only coordinates whose Locations line
(the first that gives coordinates, the one activity-store took them from) has
a basis in `-bases` are read: `From your places (Home)` is the saved address
and `Based on your past activity` a guess, not where the user was then.

`~/bin/location-import-photo-sidecars -dir <dir> [-date-sources exif,filename,google_photos]
[-dry-run]` reads every `sidecars.jsonl` under `<dir>`: one Google Takeout
photo sidecar per line, `{sha256, geoData:{latitude, longitude, …}, …}`, the
hash being the photo file's. A `geoData` of 0, 0 is no location. Each photo is
found in photo-store by `GET /photos?sha256=`; one photo-store already has GPS
for (a `photo` point), one not there, and one not dated by a trusted source
are left out and counted. The rest become points, source
`google_photos_sidecar`, `source_ref` the photo id, at the photo's
`taken_at`, with no accuracy. Both imports are found again by the hash of the
points they would write.
