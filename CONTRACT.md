# calendar-store contract

`127.0.0.1:8342` (`CALENDAR_STORE_ADDR`), no auth: keep it on loopback and put a front door in front. JSON everywhere except the body of an .ics import, which is the iCalendar file itself.

## Ids

| Record | Id |
|---|---|
| calendar | `calendar_000001` |
| calendar event | `calendar_event_000001` (kanban-store's entity-type registry claims it, so it draws as a chip) |
| import | `calendar_import_000001` |
| person | people-store's `person_000001`, never made here |

## Routes

| Method | Path | Body / query | Answer |
|---|---|---|---|
| GET | `/health` | | `{status, counts:{calendars, events, people_links, imports}}` |
| GET | `/settings` | | llm-bridge `servicesettings` description |
| GET | `/vocabularies` | | `{link_sources, revision_reasons}` |
| GET | `/calendars` | | `[Calendar]` in the order made |
| POST | `/calendars` | `{name, source, source_ref}` | **201** the new calendar, or **200** the one with that `source` and `source_ref` |
| POST | `/calendars/{id}/imports` | the `.ics` file; `?file_name=` | **201** `Import {id, calendar_id, file_name, format, file_sha256, created, updated, unchanged, people_linked, skipped, imported_at}`. One transaction: a file that does not parse writes nothing (400). Unknown calendar 404 |
| POST | `/calendars/{id}/google-event-imports` | a JSON array of Google Calendar v3 event resources, as Google lists them with `singleEvents=false` (dash: `GET /api/calendar/google?calendarId=…&singleEvents=false&showDeleted=true`); `?file_name=` | **201** `Import` with `format` `google-event-json`. Same rules as an .ics import, below. The calendar's `source` must be `google` (400). A body that does not parse, an event with no `iCalUID` or no start, or one event twice writes nothing (400). A Google id that names a different `(uid, recurrence_id)` than the row it is stored on is 409 |
| GET | `/imports` | | `[Import]` newest first |
| GET | `/events` | `calendar_id`, `from` (inclusive), `to` (exclusive) on `starts_at`, `q` (substring of summary, location or description), `person_id`, `limit` (default 100, at most 1000; more is 400), `offset` | `[Event]` oldest first, without `raw` |
| GET | `/events/{id}` | | `Event` with `raw` |
| GET | `/events/{id}/revisions` | | `[{revision_id, reason, replaced_at, event}]` newest first |
| POST | `/events/{id}/people` | `{person_id, linked_by, reason}` | **201** the event. `linked_by` is `agent` (reason required) or `operator`; `attendee-email` links are the import's. The person must be live in people-store. Already linked 409 |
| DELETE | `/events/{id}/people/{person_id}` | | **204**. An `attendee-email` link comes back at the next import that changes the event |

## Event

`{id, calendar_id, uid, recurrence_id, source_event_id, summary, description, location, starts_at, ends_at, all_day, start_date, end_date, time_zone, recurrence_rule, status, transparency, organizer_email, organizer_name, source_created_at, source_modified_at, sequence, raw?, import_id, created_at, updated_at, attendees:[{email, name, role, participation_status}], people:[{person_id, linked_by, reason, linked_at}]}`

- `uid` and `recurrence_id` are the source's key: an import finds an event again by them within its calendar. `recurrence_id` is `''` for a single event or a series, and `<TZID>:<value>` or `<value>` for a changed copy of one date of a series.
- Times are epoch seconds. A timed event keeps the zone it was written in (`time_zone`: a TZID, `UTC`, or the calendar's `X-WR-TIMEZONE` for a floating time; a floating time in a calendar that names no zone is refused). An all-day event has `all_day` true, `start_date` and `end_date` (exclusive, as iCalendar has it), and `starts_at`/`ends_at` at UTC midnight of those dates, for ordering only.
- `recurrence_rule` is the RRULE as written. The store does not expand series.
- `raw` is the whole VEVENT as the file wrote it. An import that brings the same text changes nothing; different text copies the old row to revisions first. An event a later file leaves out is kept.

## Google event imports

- An event is found first by `source_event_id` (Google's `id`), then by `(uid, recurrence_id)`: `uid` is `iCalUID`, and `recurrence_id` is `originalStartTime` written as Google's .ics export writes RECURRENCE-ID (`<timeZone>:<local time>`, a UTC time with `Z`, or a date). That is how an event an .ics export brought is found and given its Google id instead of being stored twice. A later .ics import keeps the Google id.
- `raw` is the event resource as JSON. The first Google import of an event an .ics import brought changes its `raw`, so the .ics row goes to revisions.
- Columns are filled as the .ics parser fills them: `status`, `transparency` and attendee `participation_status` in iCalendar's upper case (Google leaves out `transparency` when it is `opaque`, its default), attendee `role` `OPT-PARTICIPANT` when Google marks the attendee optional, `recurrence_rule` from the `RRULE:` line of `recurrence`. `time_zone` is `start.timeZone`.
- A cancelled date of a series arrives with only `originalStartTime`; it is stored as its own row with that start and status `CANCELLED`. An event deleted from the calendar may arrive with only its id: the row with that Google id is marked `CANCELLED` and keeps every other column; one the store never held is counted in `skipped`.
