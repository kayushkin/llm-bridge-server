# journal-store

Keeps your own writing: journal entries, blog posts, notes, notes about one
person or thing, timelines, and transcripts of conversations you took part in.
Everything is private by default.

It runs inside [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server),
which serves its routes at `/journal-store/` and keeps its records in Postgres
(schema `journal`, role `journal_module`). Until then it was a service of its
own over SQLite; `ImportStandaloneDatabase` moves such a database in. Each
entry has an id like `entry_000001`, so an agent's note, a timeline or another
store can cite it, and [bridge-ui](https://github.com/kayushkin/bridge-ui)
draws that id as a chip, using the TypeScript types in `ts/`.

Every record has an owner, the principal who keeps it. A person reaches only
their own records; an administrator reaches everyone's, or one person's with
`?owner_principal_id=`. A caller that names no principal gets nothing.

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
- **Import** notes and posts: a directory of exported notes (`.txt`, `.md`,
  `.docx`, `.pdf`, with Samsung Notes file names understood) and a Facebook
  export's posts. An import matches entries by
  where they came from, so running it again changes only what changed, and it
  never touches what you decided about publishing.
- **Promote a todo.** A checklist line in an entry can become a
  [noteboard](https://github.com/kayushkin/noteboard) todo, and the line then
  links to it. noteboard is open to every agent on the host, so only an
  administrator may do this, and only with their own entries.

[`CONTRACT.md`](CONTRACT.md) lists every route and field.

## Use it

Through llm-bridge-server, as a signed-in person (the server decides who the
request acts as; `$B` is the server's address plus `/journal-store`):

```sh
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

## Test

The tests run against a throwaway Postgres, from the repository root:

```sh
export POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh start)"
go test ./internal/connectedrecords/journalstore/...
```

## Importing

Every importer writes private entries, takes `-dry-run` to ask the store what
it would do without writing, and `-h` for its flags. They are HTTP clients:
by default they call the standalone store's old address,
`http://127.0.0.1:8322`, and a URL flag (`-store-url` or `-journal-store-url`)
points them elsewhere.

```sh
journal-import-files -time-zone Europe/Lisbon -dry-run <directory>
journal-import-facebook-posts -activity <export>/your_facebook_activity -self-name '<your name>'
journal-point-transcripts-at-messages -dry-run
journal-timeline-calendar-events -h
journal-timeline-discord-events -h
```

`journal-import-files` reads `.txt`, `.md`, `.docx` and (with `pdftotext`
installed) `.pdf`; a file that should read and does not stops the run before
anything is written. The last three place records from message-store,
calendar-store and discord-signup-store on transcripts and timelines.

## License

MIT. See `LICENSE`.
