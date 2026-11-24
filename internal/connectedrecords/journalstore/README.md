# journal-store

A small Go service that keeps your own writing: journal entries, blog posts,
notes, notes about one person or thing, timelines, and transcripts of
conversations you took part in. Everything is private by default and lives in
SQLite behind a loopback HTTP API.

It is one of the stores of the [llm-bridge](https://github.com/kayushkin/llm-bridge)
ecosystem. Each entry has an id like `entry_000001`, so an agent's note, a
timeline or another store can cite it, and
[bridge-ui](https://github.com/kayushkin/bridge-ui) draws that id as a chip,
using the TypeScript types in `ts/`. It works on its own too: it is a plain
HTTP API.

## What it does

- **Mentions by id.** A markdown link with the `ref:` scheme,
  `[Carol](ref:person_000012)`, is a mention of `person_000012`. Every write
  records it, and `GET /mentions?ref=person_000012` lists the entries that
  mention Carol. The link text is only what was shown when it was written.
  Which kind of record an id is comes from
  [reference-store](https://github.com/kayushkin/reference-store)'s list of
  reference types.
- **Keep history.** `DELETE` is soft, and every update, delete, restore and
  import update first copies the whole row to a revision.
- **Publish.** An entry is `private`, `draft` or `published`; published ones
  get a slug and are served at `GET /published/{slug}` for a public site.
- **Timelines.** A timeline is an entry whose dated items cite the entries,
  messages or notes they rest on; several timelines can be merged in time
  order.
- **Import** notes and chat archives: a directory of exported notes (`.txt`,
  `.md`, `.docx`, `.pdf`, with Samsung Notes file names understood), a NirSoft
  SkypeLogView export, a Discord data package's direct messages, and a
  Facebook export's Messenger chats and posts. An import matches entries by
  where they came from, so running it again changes only what changed, and it
  never touches what you decided about publishing.
- **Promote a todo.** A checklist line in an entry can become a
  [noteboard](https://github.com/kayushkin/noteboard) todo, and the line then
  links to it.

[`CONTRACT.md`](CONTRACT.md) lists every route and field.

## Build

journal-store reads its settings through llm-bridge's `servicesettings`
package, and `go.mod` points at a checkout beside this one. The
`sqlite_fts5` build tag is required: without it the binary builds and then
dies at start with `no such module: fts5`.

```sh
git clone https://github.com/kayushkin/llm-bridge
git clone https://github.com/kayushkin/journal-store
cd journal-store
go test -tags sqlite_fts5 ./...
go build -tags sqlite_fts5 ./cmd/...
```

`make check` runs gofmt, vet, the tests and the build with the tag set.

## Run

```sh
./journal-store   # listens on 127.0.0.1:8322
curl -s localhost:8322/health
```

| Variable | Default | |
|---|---|---|
| `JOURNAL_STORE_ADDR` | `127.0.0.1:8322` | listen address |
| `JOURNAL_STORE_DATA_DIR` | `~/.config/journal-store` | holds `journal-store.db` (directory 0700, file 0600) |
| `JOURNAL_STORE_NOTEBOARD_URL` | `http://localhost:8191` | where promote-todo creates todos |
| `JOURNAL_STORE_REFERENCE_STORE_URL` | `http://127.0.0.1:8343` | where the list of reference types is read (`GET /types`, open) |

`GET /settings` describes every setting and the value in force, never a
secret's value. A set `JOURNAL_STORE_` variable that is not in this table
stops the start, so a misspelling cannot pass silently.

**There is no authentication.** Keep it on loopback, as the default does, and
put a front door that checks who is asking in front of it if anyone else
should reach it.

To run it as a systemd user service, copy the binary to `~/bin` and
`journal-store.service` to `~/.config/systemd/user/`, then
`systemctl --user enable --now journal-store`.

## Use it

```sh
B=http://127.0.0.1:8322
curl -s -X POST $B/entries -H 'Content-Type: application/json' \
  -d '{"kind":"journal","body":"Lunch with [Carol](ref:person_000012).\n\n- [ ] book the dentist"}'
curl -s "$B/entries?q=lunch"
curl -s "$B/mentions?ref=person_000012"                                    # backlinks
curl -s -X POST $B/entries/entry_000001/promote-todo -d '{"line":3}'       # line 3 becomes a noteboard todo
curl -s -X PATCH $B/entries/entry_000001 -d '{"tags":["food"]}'
curl -s -X DELETE $B/entries/entry_000001                                  # soft
curl -s -X POST $B/entries/entry_000001/restore
curl -s $B/entries/entry_000001/revisions

curl -s -X POST $B/entries -d '{"kind":"post","title":"Hello, world","body":"…","visibility":"published"}'
curl -s $B/published/hello-world
```

## Importing

Every importer writes private entries, takes `-dry-run` to ask the store what
it would do without writing, and `-h` for its flags.

```sh
journal-import-files -time-zone Europe/Lisbon -dry-run <directory>
journal-import-skype -log <export.txt.gz> -time-zone America/Chicago -self <your skype name>
journal-import-discord-package -messages <package>/Messages -self-id <your id> -self-name <name> \
    -time-zones 'Europe/Lisbon,2021-07-11=America/Chicago'
journal-import-facebook-messages -messages <export>/your_facebook_activity/messages \
    -self-name '<your name>' -time-zones Europe/Lisbon
journal-import-facebook-posts -activity <export>/your_facebook_activity -self-name '<your name>'
```

`-time-zones` says where you lived, so a message is filed under the day it was
where you were: the first zone, then `day=zone` for each move.
`journal-import-files` reads `.txt`, `.md`, `.docx` and (with `pdftotext`
installed) `.pdf`; a file that should read and does not stops the run before
anything is written. The chat importers keep direct messages and small group
chats only.

## License

MIT. See `LICENSE`.
