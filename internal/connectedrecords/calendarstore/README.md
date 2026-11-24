# calendar-store

Calendar events as their calendars wrote them, kept whole, with who each was with as ids from a people service.

- Import an iCalendar (`.ics`) file into a calendar; importing it again changes only what changed, and every earlier version of an event is kept.
- Each event keeps its attendees, and links to people: made from attendees' addresses at import, or recorded with a reason by an agent or a person.
- Events have ids (`calendar_event_000001`) other services can cite.

Go, SQLite, one HTTP service on loopback. `CONTRACT.md` is the route table.

```bash
go test ./... && go build ./cmd/calendar-store
curl -s -X POST localhost:8342/calendars -d '{"name":"Main","source":"google","source_ref":"main"}'
curl -s -X POST --data-binary @calendar.ics 'localhost:8342/calendars/calendar_000001/imports?file_name=calendar.ics'
```

MIT licensed.
