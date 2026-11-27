# people-store

The people in one person's own life — friends, family, coworkers, everyone
they chat with — with their chat handles, emails, phone numbers and how they
relate to each other. Each person gets an id like `person_000001` that other
records join on; names ride alongside for display only.

It runs inside [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server)
at `/people/`, with its records in Postgres (schema `people`), and is
one of the connected-records stores there: every record has an owner, the
principal who keeps it, and a caller reaches only their own records unless
they are an administrator. A journal entry, a chat message or an agent's note
cites a person by id, and [bridge-ui](https://github.com/kayushkin/bridge-ui)
can draw that id as a chip using the TypeScript types in `ts/`.

## What it does

- **Persons** with a display name, a birthday (with or without the year), how
  you met, tags, and an optional link to a login in
  principal-store.
- **Handles** (`email`, `phone`, `matrix_user`, `discord`, `url`), stored
  normalized. One handle belongs to at most one of an owner's persons, and every link records
  who made it and why. Removing a handle from a person records a refusal, so
  no automatic source puts it back.
- **Relationships**, stored once per fact and shown from each side with the
  inverse kind (parent and child, sibling, partner, friend, coworker).
- **Suggestions**: one queue of guesses that some handles are one person.
  Nothing is attached until someone accepts. `people-import-multichat` fills
  it from [multichat](https://github.com/kayushkin/multichat)'s contacts and
  reports how much each handle talks, so the people you talk to most come first.
- **A review queue** for an agent that links or suggests handles in batches,
  with claims so two sessions never take the same ones.
- **No hard delete, and revisions.** A deleted person still answers
  `GET /persons/{id}`, so whatever points at them keeps resolving, and every
  change keeps the person as they were.
- **Full-text search** over names, how you met, tags and handles.
- **An owner on every record.** Two people on one server each keep their own
  address book; one handle may be a person in each.

`CONTRACT.md` lists every route and field.

## Build and test

It builds with the rest of llm-bridge-server. The tests need a Postgres to make
throwaway databases in; the repo's `scripts/test-postgres.sh` starts one:

```sh
export POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh start)"
go test ./internal/connectedrecords/people/...
```

To move a standalone people-store's SQLite file in, the server calls
`ImportStandaloneDatabase` with a copy of the file (`sqlite3 .backup`) and the
principal who will own every record.

## Use it

Through the server, as a signed-in person (`$BRIDGE` is its address):

```sh
curl -s -X POST $BRIDGE/people/persons -H 'Content-Type: application/json' \
  -d '{"display_name":"Alice Smith","birthday":"--05-14","how_met":"book club","tags":["neighbors"]}'
curl -s -X POST $BRIDGE/people/persons/person_000001/identities \
  -d '{"kind":"email","value":"Alice@Example.com","linked_by":"operator"}'
curl -s '$BRIDGE/people/identities/resolve?kind=email&value=alice@example.com'
curl -s -X POST $BRIDGE/people/persons/person_000001/relationships -d '{"person_id":"person_000002","kind":"sibling"}'
curl -s '$BRIDGE/people/persons?q=ali'
```

## Importing from multichat

`people-import-multichat` reads multichat's unified contact list, proposes one
suggestion per contact group, and reports each handle's activity. It needs
`PEOPLE_STORE_URL`, `MULTICHAT_URL`, `AUTH_STORE_URL` and `AUTH_STORE_TOKEN`
(multichat's API token is read from
auth-store as provider `multichat`).
Then review:

```sh
curl -s '$BRIDGE/people/suggestions?status=proposed&sort=activity'
curl -s -X POST $BRIDGE/people/suggestions/suggestion_000001/accept -d '{}'
```

## License

MIT. See `LICENSE`.
