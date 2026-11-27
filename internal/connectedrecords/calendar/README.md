# calendar-store

Calendar events as their calendars wrote them, kept whole, with who each was with as ids from a people service.

- Import an iCalendar (`.ics`) file into a calendar; importing it again changes only what changed, and every earlier version of an event is kept.
- Each event keeps its attendees, and links to people: made from attendees' addresses at import, or recorded with a reason by an agent or a person.
- Events have ids (`calendar_event_000001`) other services can cite.
- Every record has an owner (`owner_principal_id`), and a caller reaches only its own unless it sees every owner.

It runs inside llm-bridge-server, at `/calendar-store/`, on Postgres (schema `calendar`). `CONTRACT.md` is the route table and the owner rules. The server builds the store with `New(db, people)`, where `people` answers questions about one owner's address book, and mounts it with `RegisterHandlers`.

```bash
POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh url)" go test ./internal/connectedrecords/calendar/...
```

MIT licensed.
