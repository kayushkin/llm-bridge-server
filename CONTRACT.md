# calendar-store contract

`127.0.0.1:8342` (`CALENDAR_STORE_ADDR`), no auth: keep it on loopback and put a front door in front. JSON everywhere except the body of an import, which is the iCalendar file itself.

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
| POST | `/calendars/{id}/imports` | the `.ics` file; `?file_name=` | **201** `Import {id, calendar_id, file_name, file_sha256, created, updated, unchanged, people_linked, imported_at}`. One transaction: a file that does not parse writes nothing (400). Unknown calendar 404 |
| GET | `/imports` | | `[Import]` newest first |
| GET | `/events` | `calendar_id`, `from` (inclusive), `to` (exclusive) on `starts_at`, `q` (substring of summary, location or description), `person_id`, `limit` (default 100, at most 1000; more is 400), `offset` | `[Event]` oldest first, without `raw` |
| GET | `/events/{id}` | | `Event` with `raw` |
| GET | `/events/{id}/revisions` | | `[{revision_id, reason, replaced_at, event}]` newest first |
| POST | `/events/{id}/people` | `{person_id, linked_by, reason}` | **201** the event. `linked_by` is `agent` (reason required) or `operator`; `attendee-email` links are the import's. The person must be live in people-store. Already linked 409 |
| DELETE | `/events/{id}/people/{person_id}` | | **204**. An `attendee-email` link comes back at the next import that changes the event |

## Event

`{id, calendar_id, uid, recurrence_id, summary, description, location, starts_at, ends_at, all_day, start_date, end_date, time_zone, recurrence_rule, status, transparency, organizer_email, organizer_name, source_created_at, source_modified_at, sequence, raw?, import_id, created_at, updated_at, attendees:[{email, name, role, participation_status}], people:[{person_id, linked_by, reason, linked_at}]}`

- `uid` and `recurrence_id` are the source's key: an import finds an event again by them within its calendar. `recurrence_id` is `''` for a single event or a series, and `<TZID>:<value>` or `<value>` for a changed copy of one date of a series.
- Times are epoch seconds. A timed event keeps the zone it was written in (`time_zone`: a TZID, `UTC`, or the calendar's `X-WR-TIMEZONE` for a floating time; a floating time in a calendar that names no zone is refused). An all-day event has `all_day` true, `start_date` and `end_date` (exclusive, as iCalendar has it), and `starts_at`/`ends_at` at UTC midnight of those dates, for ordering only.
- `recurrence_rule` is the RRULE as written. The store does not expand series.
- `raw` is the whole VEVENT as the file wrote it. An import that brings the same text changes nothing; different text copies the old row to revisions first. An event a later file leaves out is kept.
