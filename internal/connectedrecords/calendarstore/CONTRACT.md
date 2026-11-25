# calendar-store contract

calendar-store runs inside llm-bridge-server, at `/calendar-store/` with the paths below, and keeps its records in Postgres (schema `calendar`, role `calendar_module`). It answers only a request carrying the caller the server verified; one with none is 401. JSON everywhere except the body of an .ics import, which is the iCalendar file itself.

## Owners

Every record has an owner, `owner_principal_id` (principal-store's `principal_000001`), the person who keeps it.

- `POST /calendars` makes a calendar owned by the caller, or answers 200 with the caller's own calendar with that `source` and `source_ref`. The pair is unique per owner, so two people may each keep a calendar with the same `source_ref`. A caller that acts as no principal cannot make one (403).
- Imports, events, attendees, people links and revisions take their calendar's owner.
- A caller reaches only its own records. A record of another owner answers 404, as a missing one does, on reads and writes alike; lists and `/health` counts hold only the caller's own. An administrator, or the server's own service, reaches every owner's, and `?owner_principal_id=` narrows any read to one owner. Anyone else may name only itself there (403 otherwise).
- An import asks each attendee's address of the **event owner's** people records, and `POST /events/{id}/people` checks the person in the event owner's records: a person only another owner keeps is refused (400) as an unknown one is, whoever asks.

## Ids

| Record | Id |
|---|---|
| calendar | `calendar_000001` |
| calendar event | `calendar_event_000001` (reference-store lists it, so it draws as a chip) |
| import | `calendar_import_000001` |
| person | people-store's `person_000001`, never made here |

## Routes

| Method | Path | Body / query | Answer |
|---|---|---|---|
| GET | `/health` | | `{status, counts:{calendars, events, people_links, imports}}`, counted in the caller's reach |
| GET | `/vocabularies` | | `{link_sources, revision_reasons}` |
| GET | `/calendars` | | `[Calendar {id, owner_principal_id, name, source, source_ref, created_at}]` in the order made |
| POST | `/calendars` | `{name, source, source_ref}` | **201** the new calendar, or **200** the one with that `source` and `source_ref` |
| POST | `/calendars/{id}/imports` | the `.ics` file; `?file_name=` | **201** `Import {id, owner_principal_id, calendar_id, file_name, format, file_sha256, created, updated, unchanged, people_linked, skipped, imported_at}`. One transaction: a file that does not parse writes nothing (400). Unknown calendar, or one the caller cannot reach, 404 |
| POST | `/calendars/{id}/google-event-imports` | a JSON array of Google Calendar v3 event resources, as Google lists them with `singleEvents=false` (dash: `GET /api/calendar/google?calendarId=…&singleEvents=false&showDeleted=true`); `?file_name=` | **201** `Import` with `format` `google-event-json`. Same rules as an .ics import, below. The calendar's `source` must be `google` (400). A body that does not parse, an event with no `iCalUID` or no start, or one event twice writes nothing (400). A Google id that names a different `(uid, recurrence_id)` than the row it is stored on is 409 |
| GET | `/imports` | | `[Import]` newest first |
| GET | `/events` | `calendar_id`, `from` (inclusive), `to` (exclusive) on `starts_at`, `q` (substring of summary, location or description, in any case; `%` and `_` match only themselves), `person_id`, `limit` (default 100, at most 1000; more is 400), `offset` | `[Event]` oldest first, without `raw` |
| GET | `/events/{id}` | | `Event` with `raw` |
| GET | `/events/{id}/revisions` | | `[{revision_id, owner_principal_id, reason, replaced_at, event}]` newest first. `event` is the event as it was, as JSON text kept byte for byte |
| POST | `/events/{id}/people` | `{person_id, linked_by, reason}` | **201** the event. `linked_by` is `agent` (reason required) or `operator`; `attendee-email` links are the import's. The person must be live in the event owner's people records. Already linked 409 |
| DELETE | `/events/{id}/people/{person_id}` | | **204**. An `attendee-email` link comes back at the next import that changes the event |

## Event

`{id, owner_principal_id, calendar_id, uid, recurrence_id, source_event_id, summary, description, location, starts_at, ends_at, all_day, start_date, end_date, time_zone, recurrence_rule, status, transparency, organizer_email, organizer_name, source_created_at, source_modified_at, sequence, raw?, import_id, created_at, updated_at, attendees:[{email, name, role, participation_status}], people:[{person_id, linked_by, reason, linked_at}]}`

- `uid` and `recurrence_id` are the source's key: an import finds an event again by them within its calendar. `recurrence_id` is `''` for a single event or a series, and `<TZID>:<value>` or `<value>` for a changed copy of one date of a series.
- Times are epoch seconds. A timed event keeps the zone it was written in (`time_zone`: a TZID, `UTC`, or the calendar's `X-WR-TIMEZONE` for a floating time; a floating time in a calendar that names no zone is refused). An all-day event has `all_day` true, `start_date` and `end_date` (exclusive, as iCalendar has it), and `starts_at`/`ends_at` at UTC midnight of those dates, for ordering only.
- `recurrence_rule` is the RRULE as written. The store does not expand series.
- `raw` is the whole VEVENT as the file wrote it. An import that brings the same text changes nothing; different text copies the old row to revisions first. An event a later file leaves out is kept.

## Google event imports

- An event is found first by `source_event_id` (Google's `id`), then by `(uid, recurrence_id)`: `uid` is `iCalUID`, and `recurrence_id` is `originalStartTime` written as Google's .ics export writes RECURRENCE-ID (`<timeZone>:<local time>`, a UTC time with `Z`, or a date). That is how an event an .ics export brought is found and given its Google id instead of being stored twice. A later .ics import keeps the Google id.
- `raw` is the event resource as JSON. The first Google import of an event an .ics import brought changes its `raw`, so the .ics row goes to revisions.
- Columns are filled as the .ics parser fills them: `status`, `transparency` and attendee `participation_status` in iCalendar's upper case (Google leaves out `transparency` when it is `opaque`, its default), attendee `role` `OPT-PARTICIPANT` when Google marks the attendee optional, `recurrence_rule` from the `RRULE:` line of `recurrence`. `time_zone` is `start.timeZone`.
- A cancelled date of a series arrives with only `originalStartTime`; it is stored as its own row with that start and status `CANCELLED`. An event deleted from the calendar may arrive with only its id: the row with that Google id is marked `CANCELLED` and keeps every other column; one the store never held is counted in `skipped`.

## What changed in the move into llm-bridge-server

- Postgres instead of SQLite; ids, routes and answers are as before, plus `owner_principal_id`. `GET /settings` is gone with the standalone service's own settings. An id that is not the form of its type (`/events/nonsense`) is 404.
- `q` matches as `ILIKE` with `%`, `_` and `\` escaped, so a query is a substring and nothing else. The SQLite `LIKE` read `%` and `_` in a query as wildcards, and folded case for ASCII letters only; Postgres folds case for every letter.
- `offset` below 0 is 400.
- The server's import command copies a standalone store's SQLite file in, through `ImportStandaloneDatabase`: every row keeps its id, every row gets the one owner the import names, and the numbering goes on after the highest id copied.
- `cmd/calendar-store-sync-google` still talks to the store over HTTP; its `-calendar-store-url` names where the store answers.
