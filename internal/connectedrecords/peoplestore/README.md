# people-store

A small Go service that keeps the people in your own life — friends, family,
coworkers, everyone you chat with — with their chat handles, emails, phone
numbers and how they relate to each other, in SQLite behind a loopback HTTP API.
Each person gets an id like `person_000001` that other records join on; names
ride alongside for display only.

It is one of the stores of the [llm-bridge](https://github.com/kayushkin/llm-bridge)
ecosystem. A journal entry, a chat message or an agent's note cites a person by
id, and [bridge-ui](https://github.com/kayushkin/bridge-ui) can draw that id as
a chip using the TypeScript types in `ts/`. It works on its own too: it is a
plain HTTP API.

## What it does

- **Persons** with a display name, a birthday (with or without the year), how
  you met, tags, and an optional link to a login in
  principal-store.
- **Handles** (`email`, `phone`, `matrix_user`, `discord`, `url`), stored
  normalized. One handle belongs to at most one person, and every link records
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

`CONTRACT.md` lists every route and field.

## Build

people-store reads its settings through llm-bridge's `servicesettings` package,
and `go.mod` points at a checkout beside this one. Search uses SQLite FTS5, so
**every build and test needs `-tags sqlite_fts5`**: a binary built without it
compiles and then dies at boot with `no such module: fts5`. The `Makefile` sets
the tag.

```sh
git clone https://github.com/kayushkin/llm-bridge
git clone https://github.com/kayushkin/people-store
cd people-store
make test
make build      # ./people-store and ./people-import-multichat
```

## Run

```sh
PEOPLE_STORE_PRINCIPAL_STORE_URL=http://127.0.0.1:8314 ./people-store   # listens on 127.0.0.1:8321
curl -s localhost:8321/health
```

| Variable | Default | |
|---|---|---|
| `PEOPLE_STORE_ADDR` | `127.0.0.1:8321` | listen address |
| `PEOPLE_STORE_DATA_DIR` | `~/.config/people-store` | holds `people-store.db` |
| `PEOPLE_STORE_PRINCIPAL_STORE_URL` | required | principal-store, which checks every `principal_id` written |

`GET /settings` describes every setting and the value in force. A set
`PEOPLE_STORE_` variable that is not declared stops the service at boot.

**There is no authentication.** Keep it on loopback, as the default does, and
put a front door that checks who is asking in front of it if anyone else
should reach it. This is an address book.

To run it as a systemd user service, copy the binary to `~/bin` and
`people-store.service` to `~/.config/systemd/user/`, then
`systemctl --user enable --now people-store`.

## Use it

```sh
curl -s -X POST localhost:8321/persons -H 'Content-Type: application/json' \
  -d '{"display_name":"Alice Smith","birthday":"--05-14","how_met":"book club","tags":["neighbors"]}'
curl -s -X POST localhost:8321/persons/person_000001/identities \
  -d '{"kind":"email","value":"Alice@Example.com","linked_by":"operator"}'
curl -s 'localhost:8321/identities/resolve?kind=email&value=alice@example.com'
curl -s -X POST localhost:8321/persons/person_000001/relationships -d '{"person_id":"person_000002","kind":"sibling"}'
curl -s 'localhost:8321/persons?q=ali'
```

## Importing from multichat

`people-import-multichat` reads multichat's unified contact list, proposes one
suggestion per contact group, and reports each handle's activity. It needs
`PEOPLE_STORE_URL`, `MULTICHAT_URL`, `AUTH_STORE_URL` and `AUTH_STORE_TOKEN`
(multichat's API token is read from
auth-store as provider `multichat`).
Then review:

```sh
curl -s 'localhost:8321/suggestions?status=proposed&sort=activity'
curl -s -X POST localhost:8321/suggestions/suggestion_000001/accept -d '{}'
```

## License

MIT. See `LICENSE`.
