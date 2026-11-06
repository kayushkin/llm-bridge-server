# About calendar-store

## What it owns

`127.0.0.1:8342`, loopback only. Calendar events as their calendars wrote them: every VEVENT of an imported iCalendar file, kept whole (`raw`), with its attendees and the people it was with as people-store ids. Ids `calendar_000001`, `calendar_event_000001`, `calendar_import_000001`; kanban-store's entity-type registry claims `calendar_event_…`, so a journal timeline or a note cites an event as a chip. `CONTRACT.md` is the route table. **Calendars are private**: never copy an event's description into noteboard or a repo.

# How it works

## Imports and revisions

`POST /calendars/{id}/imports` takes the `.ics` file as the body and runs in one transaction. An event is found again by `(calendar_id, uid, recurrence_id)`; the same raw text changes nothing, different text copies the whole old row to `calendar_event_revisions` first, and an event the newer file leaves out is kept. Series are stored as written (`recurrence_rule`); a changed copy of one date is its own row with a `recurrence_id`. The store does not expand series: a reader that needs dates expands the rule.

## People

`calendar_event_people` links an event to `person_…` ids. `attendee-email` links are made at import by asking people-store (`CALENDAR_STORE_PEOPLE_STORE_URL`, `GET /identities/resolve?kind=email`) about each attendee, and are made again whenever the event changes. An agent links a person named only in a title with `POST /events/{id}/people {"person_id","linked_by":"agent","reason"}`; the store checks the person is live in people-store. Never link by a name match alone: find the person in people-store, and say in `reason` what in the event names them.

# Access and operations

## Unit and data

Unit `calendar-store.service` (user unit), binary `~/bin/calendar-store`, data `~/.config/calendar-store/calendar-store.db` (mode 600). Every variable is declared in `settings.go` and served at `GET /settings`; a set `CALENDAR_STORE_` variable nobody declared stops the start, and `deploy.sh` checks the running unit's environment against the declarations first.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/calendar-store`, package `calendarstore`, server in `cmd/calendar-store`. `go test ./...`; deploy only through `./deploy.sh` from the main clone on `main`. The repo may become public: test data is made up, and nothing about the operator's calendars goes in code, tests or this prompt. Where this host's calendars came from is in a noteboard note tagged `calendar-store`.
