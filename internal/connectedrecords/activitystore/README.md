# activity-store

A small Go service that keeps what you did online: searches, pages visited,
videos watched, apps used and places looked up on a map, one activity per
thing done at one time, in SQLite behind a loopback HTTP API. It imports
Google Takeout: My Activity's pages, YouTube's watch and search history, and
Chrome's history and bookmarks.

It is one of the stores of the [llm-bridge](https://github.com/kayushkin/llm-bridge)
ecosystem. Each activity has an id like `activity_000001`, so a note or a
journal timeline can cite it, and [bridge-ui](https://github.com/kayushkin/bridge-ui)
draws that id as a chip using the TypeScript types in `ts/`. It works on its
own too: it is a plain HTTP API.

## What it does

- **Import** a Takeout export. Each file is found again by its SHA-256, so
  importing it twice changes nothing, and an import that stopped part way
  resumes when run again. Voice recordings and pictures that come with a card
  are kept as files beside the database.
- **Fold** the same thing seen twice: a YouTube watch in My Activity and in
  YouTube's history, or a page in My Activity and in Chrome's history, is one
  activity with two sightings.
- **Search** every word of the titles, links and details (SQLite FTS5).
- **Summarize a day** in any time zone, by product (`GET /days/2026-01-05?tz=Europe/Paris`).
- **Keep history.** Deletes are soft and leave a revision; a purge removes
  what an activity said, everywhere, for something that must not be kept.

`CONTRACT.md` lists every route and field.

## Build

activity-store reads its settings through llm-bridge's `servicesettings`
package, so `go.mod` points at a checkout beside this one. The FTS5 table
needs the `sqlite_fts5` build tag:

```sh
git clone https://github.com/kayushkin/llm-bridge
git clone https://github.com/kayushkin/activity-store
cd activity-store
go test -tags sqlite_fts5 ./...
go build -tags sqlite_fts5 ./cmd/activity-store ./cmd/activity-import-google
```

## Run

```sh
./activity-store            # listens on 127.0.0.1:8336
curl -s localhost:8336/health
```

| Variable | Default | |
|---|---|---|
| `ACTIVITY_STORE_ADDR` | `127.0.0.1:8336` | listen address |
| `ACTIVITY_STORE_DATA_DIR` | `~/.config/activity-store` | the database and attachments |

`GET /settings` describes every setting and the value in force.

**There is no authentication.** Keep it on loopback, as the default does, and
put a front door that checks who is asking in front of it if anyone else
should reach it.

## Importing

```sh
activity-import-google -dir Takeout -dry-run   # say what it would read
activity-import-google -dir Takeout
```

The command posts to `ACTIVITY_STORE_URL` (default `http://127.0.0.1:8336`).

## License

MIT. See `LICENSE`.
