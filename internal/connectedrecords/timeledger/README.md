# timeledger (formerly time-ledger-store)

`internal/connectedrecords/timeledger`, package `timeledger`. It was the standalone time-ledger-store until it moved into llm-bridge-server; on 2026-11-27 its package, route prefix, Postgres module, settings and import subcommand took the name `timeledger`. Its record ids did not change.

An account of what someone did, when and where: time entries over spans of time, each linked by id to the records that brought it in (a calendar event, a Discord event, an event suggestion, a message, a journey) and the records that show whether it happened (visits, photos, charges, attendance records), and reviewed by a model and by the person.

- Entries overlap freely and nest: a week away holds a day at a building, which holds an hour of work there. A child lies inside its parent's span.
- An entry carries any number of kinds (`dance`, `social`, `work`…), from a list anyone can add to.
- Each kind of linked record has its own table, keyed by the id of the store that owns it.
- What the person decided is never undone by a model, and what a model decided is never undone by a program.
- Every review is kept; the person's newest stands, else the model's newest, else the outcome is unknown.

- Every record has an owner (`owner_principal_id`), and a caller reaches only its own unless it sees every owner.

It runs inside llm-bridge-server, at `/time-ledger/`, on Postgres (schema `time_ledger`). `CONTRACT.md` is the route table and the owner rules. The server builds the store with `New(db)` and mounts it with `RegisterHandlers`. Three commands talk to it over HTTP: `time-ledger-import` (makes entries from the sources), `time-ledger-gather-evidence` (links visits and photos) and `time-ledger-review` (asks a model whether each entry happened).

```bash
POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh url)" go test ./internal/connectedrecords/timeledger/...
```

MIT licensed.
