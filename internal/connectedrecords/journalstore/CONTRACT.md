# journal-store — API contract

`127.0.0.1:8322`. The user's own writing: journal entries, blog posts, notes,
and notes about one person or thing. Kept apart from noteboard, which is mostly
agent output and open to every agent on the host.

Module `github.com/kayushkin/journal-store`, root package `journalstore`,
server in `cmd/journal-store` installed at `~/bin/journal-store`, file importer
in `cmd/journal-import-files` installed at `~/bin/journal-import-files`. SQLite
at `<data dir>/journal-store.db` (default `~/.config/journal-store`, directory
0700, file 0600), WAL, foreign keys on.

Routes are rooted at `/`; dash adds its prefix and the auth this service has
none of. All timestamps are epoch seconds (`INTEGER`, 0 = unset).

**The bind is `127.0.0.1`, and it is part of this contract.** No auth, private
writing: the unit sets `JOURNAL_STORE_ADDR=127.0.0.1:8322`, `settings.go`
defaults to the same, and `deploy.sh` fails if the port listens anywhere else.

Build with `-tags sqlite_fts5`, or the binary starts and dies at the first
boot with `no such module: fts5`.

## Settings

Declared once in `settings.go`, served read-only at `GET /settings`.

| Variable | Default | What it is |
|---|---|---|
| `JOURNAL_STORE_ADDR` | `127.0.0.1:8322` | listen address |
| `JOURNAL_STORE_DATA_DIR` | `~/.config/journal-store` | holds `journal-store.db` |
| `JOURNAL_STORE_NOTEBOARD_URL` | `http://localhost:8191` | where promote-todo creates todos |
| `JOURNAL_STORE_REFERENCE_STORE_URL` | `http://127.0.0.1:8343` | where the list of reference types is read (`GET /types`, open) |

---

## `entries`

| Field | Meaning |
|---|---|
| `id` | `entry_000001`. What anything else joins on |
| `kind` | `GET /entry-kinds`: `journal`, `post`, `note`, `about`, `timeline`, `transcript` |
| `title` | required, except for `journal` |
| `body` | markdown |
| `written_at` | the day the entry is about or was written. **Now only when the creating call sent none**; `POST /imports` must send it |
| `visibility` | `GET /visibilities`: `private` (default), `draft`, `published` |
| `published_at` | the **first** time it became published. Going back to draft keeps it |
| `slug` | lowercase letters (any script) and digits joined by single hyphens. **Unique among live published entries**; drafts and private entries may hold any. Derived from the title when an entry is published with none; editable by `PATCH` |
| `tags` | JSON array of strings, trimmed, deduplicated, never empty strings |
| `about_ref` | `kind` `about`, where it is required, and `kind` `timeline`, where it is optional: the id of what it is about or follows (`person_000012`). Any other kind with one is a 400. `GET /entries?about_ref=` lists them |
| `source`, `source_ref`, `source_hash` | written by `POST /imports` only. `(source, source_ref)` is unique |
| `created_at`, `updated_at` | |
| `deleted_at` | 0 = live. `DELETE` sets it; `POST /entries/{id}/restore` clears it |
| `mentions` | `GET /entries/{id}` only: the entry's mentions, below |

## Mentions: `[text](ref:<id>)`

A markdown link whose target uses the `ref:` scheme is a mention of `<id>`:
`[Carol](ref:person_000012)`, `[that todo](ref:<noteboard uuid>)`,
`[thread](ref:principal_000003)`. **The id is the truth; the link text is
only what was shown when it was written.** On every create, update, import and
promote-todo the body is parsed and the entry's rows in `mentions` are
rewritten: `entry_id`, `position` (0-based order), `line` (1-based),
`entity_ref`, `entity_type`, `label`. Links with any other target, and links
inside code spans or fenced code blocks, are not mentions.

`entity_type` comes from reference-store's list of reference types
(`GET /api/entity-types`, read with the service token at start and every five
minutes). It is the registry type whose `id_patterns` (anchored,
case-insensitive, as dash reads them) **alone** match the ref; when none or
several match it is `''`. A write never waits on reference-store: if the registry
cannot be read, the mention is stored with an empty type, `GET /health` says
`degraded` with the error, and the next good read backfills every empty type it
can classify.

## Routes

| Method | Path | |
|---|---|---|
| `GET` | `/health` | `{status, counts:{entries,deleted,published,mentions,revisions}, entity_registry:{types,fetched_at,error}}`. `status` is `degraded` when the registry has not been read |
| `GET` | `/settings` | the declared settings; the token shows only as set |
| `GET` | `/entry-kinds` | `["journal","post","note","about","timeline","transcript"]` |
| `GET` | `/visibilities` | `["private","draft","published"]` |
| `GET` | `/entries` | live entries, newest `written_at` first. `?q=` (FTS5 over title, body, tags; each word a quoted prefix, ANDed), `?kind=`, `?exclude_kind=` (repeatable: leave that kind out), `?tag=`, `?visibility=`, `?about_ref=`, `?source=` (entries `POST /imports` wrote under that source, with their `source_ref`), `?from=` (inclusive) `&to=` (exclusive) on `written_at` in epoch seconds, `?include_deleted=true`, `?limit=` (default 50, max 500), `?offset=`. A bad kind, visibility or number is a 400 |
| `POST` | `/entries` | `EntryInput`: `kind`, `title`, `body`, `written_at`, `visibility`, `slug`, `tags`, `about_ref`. **201**. Any other key is a 400 |
| `GET` | `/entries/{id}` | with `mentions`. A deleted entry is **404** unless `?include_deleted=true` |
| `PATCH` | `/entries/{id}` | any of the `POST` keys. **An unknown key is a 400** naming the accepted ones; `id`, `published_at`, `deleted_at`, `created_at`, `updated_at`, `source*` and `mentions` are 400s saying what moves them; `null` is a 400. A deleted entry is 404 |
| `DELETE` | `/entries/{id}` | soft. **204**; again on a deleted entry is a 204 that changes nothing |
| `POST` | `/entries/{id}/restore` | the entry as it was deleted. A published entry whose slug another has taken since is a **409** |
| `GET` | `/entries/{id}/revisions` | newest first, `?limit=&offset=`; works on a deleted entry |
| `POST` | `/entries/{id}/promote-todo` | `{"line": n}`, 1-based. See below. **201** `{todo_id, entry}` |
| `GET` | `/mentions?ref=<id>` | backlinks: `[{entry, mentions}]`, live entries whose body mentions exactly `<id>`, newest written first, each with its mentions of it. No `ref` is a 400 |
| `GET` | `/published` | live published entries, newest `published_at` first; `?tag=`, `?limit=&offset=` |
| `GET` | `/published/{slug}` | one live published entry, or 404 |
| `POST` | `/imports` | bulk upsert, below. `?dry_run=true` answers the same counts and writes nothing |
| `GET` | `/time-precisions` | `["minute","day","month","year"]` |
| `GET` | `/timelines/{id}/items` | the timeline's items, oldest first; `?include_deleted=true`. Not a timeline is a 400 |
| `POST` | `/timelines/{id}/items` | `{"items":[TimelineItemInput…]}`, one transaction: a bad item is a 400 naming its index and writes none. **201** `{items}`. The timeline must be a live entry of kind `timeline` |
| `GET` | `/timeline-items` | **the overlay**: `?timeline=<id>` once per timeline (at least one), `?from=` (inclusive) `&to=` (exclusive) on `occurred_at`, `?limit=` (default 500, max 2000), `?offset=`. Live items of those live timelines, merged oldest first; each carries its `timeline_id` |
| `GET` | `/timeline-items/{id}` | one item; deleted is 404 unless `?include_deleted=true` |
| `PATCH` | `/timeline-items/{id}` | any of `occurred_at`, `ends_at`, `precision`, `title`, `body`, `sources`. Unknown key, `null`, or `id`, `timeline_id`, `deleted_at`, `created_at`, `updated_at` are 400s. Deleted is 404 |
| `DELETE` | `/timeline-items/{id}` | soft, **204** |
| `POST` | `/timeline-items/{id}/restore` | the item as it was deleted |
| `GET` | `/timeline-items/{id}/revisions` | newest first, `[{revision_id, reason, replaced_at, item}]` |

## Timelines

A timeline is an entry of `kind` `timeline`, so it is an `entry_…` id like any
other and shows as a chip. `about_ref` names the person it follows; an event
timeline (a trip, a job) leaves it empty. Its dated things are
`timeline_items`:

| Field | Meaning |
|---|---|
| `id` | `timeline_item_000001` |
| `timeline_id` | the timeline entry |
| `occurred_at`, `ends_at` | epoch seconds; `ends_at` 0 for a moment, otherwise not before `occurred_at` |
| `precision` | `GET /time-precisions`: how much of `occurred_at` to believe. `month` and `year` put it on the first day |
| `title` | required |
| `body` | markdown; `[text](ref:<id>)` links are mentions |
| `sources` | ids the item rests on (a message, a note, an entry), deduplicated |

An item's body links, then its sources (with an empty `label`), are kept in
`timeline_item_mentions`. `GET /mentions?ref=` finds a timeline through a live
item's mention as well as through its own body, and `GET /entries/{id}` on a
timeline lists its body's mentions, then its items', each with `item_id`.
Items are soft-deleted and every update, delete and restore first copies the
whole row to `timeline_item_revisions`. A timeline with live items cannot
change kind (**409**).

## Revisions

Before every update, delete, restore, import update and promote-todo, the whole
row (every column of `entries`) is copied to `entry_revisions` with a
`reason` (`update`, `delete`, `restore`, `import`, `promote-todo`) and
`replaced_at`, in the same transaction as the change: a refused change leaves
no revision. `GET /entries/{id}/revisions` answers `[{revision_id, reason,
replaced_at, entry}]`. Nothing in this store is hard-deleted.

## Imports

`POST /imports {"entries":[ImportEntry…]}`. Each needs `source`,
`source_ref`, `source_hash` and `written_at`; the rest is as `POST /entries`.

- A new `(source, source_ref)` is **created**.
- A known one with the same `source_hash` is **skipped**.
- A known one with another hash is **updated**: `kind`, `title`, `body`,
  `written_at`, `tags`, `about_ref` and `source_hash` are replaced after a
  revision. **`visibility`, `slug` and `published_at` are never touched by a
  re-import** — they are the user's publishing decisions — and a deleted entry
  stays deleted.
- The whole request is one transaction. A bad entry, or one pair twice, is a
  400 and writes nothing.

Answer: `{created, updated, skipped, outcomes:[{source, source_ref, entry_id, outcome}]}`.

## promote-todo

`POST /entries/{id}/promote-todo {"line": n}` turns a markdown checklist line
(`- [ ] text`, also `*`, `+`, `1.`, indented, or ticked `[x]`) into a noteboard
todo and rewrites the line to link it: `- [ ] [text](ref:<uuid>)`, keeping the
indent, marker and tick. The todo is `{"type":"todo","title":text,"tags":["journal"],"links":[entry id]}`
with a body naming the entry and line. Line out of range, not a checklist item,
or empty: **400**. Already holds a `ref:` link: **409**. noteboard failing:
**502** and the entry is unchanged. The write lock is held across the
noteboard call, so the line cannot change in between.

## Reference chips

dash's `POST /api/resolve` probes a registry type's `get` route for an id that
matches its `id_patterns`, and treats 200 as a hit and 404 as an honest miss.
reference-store's list of types row for this store is
`{"type":"entry","service":"journal-store","get":"/entries/{id}","id_patterns":["entry_\\d{6,}"]}`.
`GET /entries/{id}` answers 200 with the entry, or 404 for an unknown or
deleted one. dash also needs `journal-store` in its `resolveServiceURLSetting`
map, with a setting holding this store's address.
